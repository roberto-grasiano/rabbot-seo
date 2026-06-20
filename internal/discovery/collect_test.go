package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// itoa is a tiny strconv.Itoa stand-in kept local so the test fixtures read
// without an extra import in the urlset-builder loop.
func itoa(i int) string { return strconv.Itoa(i) }

// newDiscTinyBody builds a Discoverer whose Sitemaps fetcher has a deliberately
// tiny MaxBodyBytes so an oversized sitemap document is Truncated, exercising the
// incomplete-on-truncation branch of the collection BFS.
func newDiscTinyBody(t *testing.T, st *store.DB, caps Caps) (*Discoverer, func()) {
	t.Helper()
	pages := fetcher.New(fetcher.Options{UserAgent: "kb/test", Timeout: 5 * time.Second, MaxBodyBytes: 50 << 20, AllowPrivate: true})
	small := fetcher.New(fetcher.Options{UserAgent: "kb/test", Timeout: 5 * time.Second, MaxBodyBytes: 256, AllowPrivate: true})
	d := &Discoverer{
		Store: st, Pages: pages, Sitemaps: small,
		Robots:  frontier.NewRobotsCache(nil, "kb/test", time.Minute),
		Resolve: func(model.Site) Caps { return caps },
		Now:     func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	return d, func() {}
}

// TestCollectAndSeedReturnsCollectedSet proves CollectAndSeed returns the
// post-budget collected URL set (the half SeedSitemaps discarded today) AND that
// the same pass admitted the new URLs (Admitted == the legacy return value).
func TestCollectAndSeedReturnsCollectedSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		body := `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`
		for _, p := range []string{"/c1", "/c2", "/c3"} {
			body += `<url><loc>` + srv.URL + p + `</loc></url>`
		}
		body += `</urlset>`
		_, _ = w.Write([]byte(body))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "cs.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: srv.URL, Name: "C", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: srv.URL, MinInterval: 600}

	d, done := newDisc(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 2000})
	defer done()
	d.Robots = frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute)

	col, err := d.CollectAndSeed(ctx, site)
	if err != nil {
		t.Fatalf("CollectAndSeed: %v", err)
	}
	if len(col.Entries) != 3 {
		t.Fatalf("Entries = %d, want 3 (collected set returned, not discarded)", len(col.Entries))
	}
	got := map[string]struct{}{}
	for _, e := range col.Entries {
		got[e.Loc] = struct{}{}
	}
	for _, p := range []string{"/c1", "/c2", "/c3"} {
		if _, ok := got[srv.URL+p]; !ok {
			t.Errorf("collected set missing %s", srv.URL+p)
		}
	}
	if col.Admitted != 3 {
		t.Errorf("Admitted = %d, want 3 (same pass seeded all three)", col.Admitted)
	}
	if col.Incomplete {
		t.Errorf("Incomplete should be false for a clean collection")
	}
	// The primary seed is the hardcoded /sitemap.xml fallback (no robots directive).
	if col.SeedURL != srv.URL+"/sitemap.xml" {
		t.Errorf("SeedURL = %q, want %s/sitemap.xml", col.SeedURL, srv.URL)
	}
}

// TestCollectAndSeedSeedStatusOK pins SeedStatus propagation: a 200 primary seed
// surfaces SeedStatus == 200.
func TestCollectAndSeedSeedStatusOK(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<url><loc>` + srv.URL + `/ok1</loc></url></urlset>`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "ss.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: srv.URL, Name: "S", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: srv.URL, MinInterval: 600}

	d, done := newDisc(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 2000})
	defer done()
	d.Robots = frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute)

	col, err := d.CollectAndSeed(ctx, site)
	if err != nil {
		t.Fatalf("CollectAndSeed: %v", err)
	}
	if col.SeedStatus != 200 {
		t.Errorf("SeedStatus = %d, want 200", col.SeedStatus)
	}
	if col.Incomplete {
		t.Errorf("a 200 single-doc collection is complete, got Incomplete=true")
	}
}

// TestCollectAndSeedSeedStatus404 proves the primary seed's HTTP status is
// surfaced even when the seed doc is a 404 (the "sitemap broke" signal): the
// status is reported, no entries are collected, and the pass is marked Incomplete
// — a 404 body is not parseable sitemap XML, so the BFS cannot confirm the URL set.
// A downstream reconcile must therefore NOT read the empty result as a mass URL
// drop (additive-only). This Incomplete contract is what the 404-status watch
// (sitemap_xml_status fires; no phantom sitemap_xml set-change) depends on.
func TestCollectAndSeedSeedStatus404(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "s404.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: srv.URL, Name: "N", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: srv.URL, MinInterval: 600}

	d, done := newDisc(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 2000})
	defer done()
	d.Robots = frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute)

	col, err := d.CollectAndSeed(ctx, site)
	if err != nil {
		t.Fatalf("CollectAndSeed: %v", err)
	}
	if col.SeedStatus != 404 {
		t.Errorf("SeedStatus = %d, want 404 (sitemap-broke signal)", col.SeedStatus)
	}
	if len(col.Entries) != 0 {
		t.Errorf("a 404 seed collects no entries, got %d", len(col.Entries))
	}
	// Pin the Incomplete contract: an unparseable 404 seed body leaves the URL set
	// unconfirmed, so the pass is Incomplete (the reconcile must stay additive-only
	// and the watch must not emit a phantom set-drop).
	if !col.Incomplete {
		t.Errorf("a 404 seed body is unparseable -> Incomplete=true (the reconcile must not read it as a mass URL drop)")
	}
}

// TestCollectAndSeedIncompleteOnChildFetchFailure is acceptance criterion 5's
// collection half: a sitemap INDEX whose child fetch errors mid-BFS yields
// Incomplete=true, while the children that DID parse are still collected and
// admitted. The index itself is a clean 200, so the failure is purely a child
// document fetch failure.
func TestCollectAndSeedIncompleteOnChildFetchFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\nSitemap: " + srv.URL + "/sitemap_index.xml\n"))
	})
	// Index points at two child sitemaps: one healthy, one that 500s.
	mux.HandleFunc("/sitemap_index.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<sitemap><loc>` + srv.URL + `/good.xml</loc></sitemap>` +
			`<sitemap><loc>` + srv.URL + `/bad.xml</loc></sitemap></sitemapindex>`))
	})
	mux.HandleFunc("/good.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<url><loc>` + srv.URL + `/g1</loc></url></urlset>`))
	})
	mux.HandleFunc("/bad.xml", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "inc.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: srv.URL, Name: "I", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: srv.URL, MinInterval: 600}

	d, done := newDisc(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 2000})
	defer done()
	d.Robots = frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute)

	col, err := d.CollectAndSeed(ctx, site)
	if err != nil {
		t.Fatalf("CollectAndSeed: %v", err)
	}
	if !col.Incomplete {
		t.Errorf("Incomplete should be true when a child sitemap fetch fails")
	}
	// The healthy child's URL was still collected and admitted (partial progress).
	if len(col.Entries) != 1 || col.Entries[0].Loc != srv.URL+"/g1" {
		t.Errorf("healthy child not collected: %+v", col.Entries)
	}
	if col.Admitted != 1 {
		t.Errorf("Admitted = %d, want 1 (the healthy child seeded despite incompleteness)", col.Admitted)
	}
	// Primary seed (the index) was a clean 200.
	if col.SeedStatus != 200 {
		t.Errorf("SeedStatus = %d, want 200 (index fetched fine)", col.SeedStatus)
	}
}

// TestCollectAndSeedIncompleteOnTruncatedDoc proves a truncated primary seed doc
// (body exceeded the fetcher cap) marks the collection Incomplete — a partial
// read must never look like a clean set.
func TestCollectAndSeedIncompleteOnTruncatedDoc(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	// Serve a large urlset that overflows the tiny MaxBodyBytes set below, so the
	// fetcher flags Truncated and the BFS skips it as a partial document.
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		body := `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`
		for i := 0; i < 200; i++ {
			body += `<url><loc>` + srv.URL + `/t` + itoa(i) + `</loc></url>`
		}
		body += `</urlset>`
		_, _ = w.Write([]byte(body))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "trunc.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: srv.URL, Name: "T", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: srv.URL, MinInterval: 600}

	d, done := newDiscTinyBody(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 2000})
	defer done()
	d.Robots = frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute)

	col, err := d.CollectAndSeed(ctx, site)
	if err != nil {
		t.Fatalf("CollectAndSeed: %v", err)
	}
	if !col.Incomplete {
		t.Errorf("Incomplete should be true when the seed doc was truncated")
	}
	if len(col.Entries) != 0 {
		t.Errorf("a truncated doc is skipped, so no entries collected, got %d", len(col.Entries))
	}
}

// TestSeedSitemapsBackCompat proves the thin SeedSitemaps wrapper still returns
// the admitted count (== col.Admitted) so existing callers are unaffected.
func TestSeedSitemapsBackCompat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<url><loc>` + srv.URL + `/w1</loc></url><url><loc>` + srv.URL + `/w2</loc></url></urlset>`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "bc.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: srv.URL, Name: "W", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: srv.URL, MinInterval: 600}

	d, done := newDisc(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 2000})
	defer done()
	d.Robots = frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute)

	added, err := d.SeedSitemaps(ctx, site)
	if err != nil {
		t.Fatalf("SeedSitemaps: %v", err)
	}
	if added != 2 {
		t.Errorf("SeedSitemaps back-compat: added=%d, want 2", added)
	}
}
