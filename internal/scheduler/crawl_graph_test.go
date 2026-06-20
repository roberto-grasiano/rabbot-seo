package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// fakeGraph records the links passed to SyncPage so a test can assert the hook
// received exactly the extractor's same-host link slice on the FetchOK path. It
// also counts calls so a test can prove the hook is NOT invoked on non-ok / 304 /
// extract-error fetches. An optional err drives the error-propagation path.
type fakeGraph struct {
	calls int
	got   [][]string
	err   error
}

func (g *fakeGraph) SyncPage(_ context.Context, _ model.Site, _ model.URL, links []string) error {
	g.calls++
	// copy the slice so a later mutation by the crawler can't taint the assertion
	cp := append([]string(nil), links...)
	g.got = append(g.got, cp)
	return g.err
}

// TestCrawlOneGraphReceivesExtractorLinks (criterion 3) proves the A9 Graph hook,
// when wired, is invoked at the same post-extract point as Discoverer on a FetchOK
// crawl and receives EXACTLY the extractor's deduped, absolute, fragment-stripped
// same-host link slice. We assert against the slice the Discoverer received (they
// must be identical), since both consume the same extract return.
func TestCrawlOneGraphReceivesExtractorLinks(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(`<html><head><title>T</title></head><body>` +
			`<a href="/a">a</a><a href="/b#frag">b</a><a href="/a">dup</a>` +
			`<a href="https://other.example/x">ext</a></body></html>`))
	}))
	defer srv.Close()

	c, _, _ := newPipeline(t)
	fd := &fakeDisc{}
	fg := &fakeGraph{}
	c.Discoverer = fd
	c.Graph = fg

	u := model.URL{ID: 1, SiteID: 1, URL: srv.URL + "/", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "")
	if res.Err != nil {
		t.Fatalf("CrawlOne err = %v", res.Err)
	}
	if res.FetchClass != model.FetchOK {
		t.Fatalf("FetchClass = %q, want ok", res.FetchClass)
	}
	if fg.calls != 1 {
		t.Fatalf("Graph.SyncPage call count = %d, want exactly 1 on a FetchOK crawl", fg.calls)
	}
	// The Graph hook must receive the SAME link slice the Discoverer got — they
	// consume one extract return. The extractor dedups, strips fragments, and drops
	// the cross-host link, so the set is deterministic: /a and /b (absolute).
	gotGraph := fg.got[0]
	if len(gotGraph) != len(fd.got) {
		t.Fatalf("Graph links %v differ in length from Discoverer links %v", gotGraph, fd.got)
	}
	for i := range gotGraph {
		if gotGraph[i] != fd.got[i] {
			t.Fatalf("Graph link[%d]=%q != Discoverer link[%d]=%q (must be the same slice)", i, gotGraph[i], i, fd.got[i])
		}
	}
	// Falsifiable content assertion: the same-host, fragment-stripped, deduped set.
	wantA, wantB := srv.URL+"/a", srv.URL+"/b"
	var sawA, sawB, sawExt, dupA int
	for _, l := range gotGraph {
		switch l {
		case wantA:
			sawA++
			dupA++
		case wantB:
			sawB++
		default:
			sawExt++
		}
	}
	if sawA != 1 {
		t.Errorf("expected /a exactly once (deduped), got %d in %v", sawA, gotGraph)
	}
	if sawB != 1 {
		t.Errorf("expected /b exactly once (fragment stripped), got %d in %v", sawB, gotGraph)
	}
	if sawExt != 0 {
		t.Errorf("cross-host link must be excluded from the graph slice, got %d in %v", sawExt, gotGraph)
	}
}

// TestCrawlOneGraphNotCalledOnNonOK (criterion 3) proves the Graph hook is NEVER
// invoked on a non-ok fetch (here a hard block): no body means no current out-set,
// so syncing would erase the page's edges to nothing on every transient block.
func TestCrawlOneGraphNotCalledOnNonOK(t *testing.T) {
	t.Parallel()
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

	c, _, _ := newPipeline(t)
	fg := &fakeGraph{}
	c.Graph = fg

	u := model.URL{ID: 2, SiteID: 1, URL: srv.URL + "/page", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "")
	if res.FetchClass != model.FetchHardBlock {
		t.Fatalf("FetchClass = %q, want hard_block", res.FetchClass)
	}
	if fg.calls != 0 {
		t.Errorf("Graph.SyncPage must NOT be called on a non-ok fetch, got %d calls", fg.calls)
	}
}

// TestCrawlOneGraphNotCalledOn304 (criterion 3) proves the Graph hook is NEVER
// invoked on a 304 Not Modified: the body is empty (the server sent no new content),
// so the page's existing edge set must be left untouched, not wiped to empty.
func TestCrawlOneGraphNotCalledOn304(t *testing.T) {
	t.Parallel()
	c, _, _ := newPipeline(t)
	// A FetchOK NotModified (304) result: ok class, no body. The graph hook gate is
	// `FetchOK && !NotModified && len(Body)>0`, so 304 must skip it.
	c.Fetcher = &fakeFetcher{res: fetcher.Result{
		FetchClass:  model.FetchOK,
		HTTPStatus:  304,
		NotModified: true,
		Body:        nil,
		Header:      http.Header{},
	}}
	fg := &fakeGraph{}
	c.Graph = fg

	// Unresolvable host so the real RobotsCache fails open without a network hop.
	u := model.URL{ID: 3, SiteID: 1, URL: "https://e.com/page", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "")
	if res.FetchClass != model.FetchOK {
		t.Fatalf("FetchClass = %q, want ok (304)", res.FetchClass)
	}
	if fg.calls != 0 {
		t.Errorf("Graph.SyncPage must NOT be called on a 304 Not Modified, got %d calls", fg.calls)
	}
}

// TestCrawlOneGraphNotCalledOnExtractError (criterion 3) proves the Graph hook is
// NEVER invoked when extraction fails: there is no valid link slice, and the page
// persisted no snapshot, so its edges must be left untouched.
func TestCrawlOneGraphNotCalledOnExtractError(t *testing.T) {
	t.Parallel()
	c, _, _ := newPipeline(t)
	c.Fetcher = &fakeFetcher{res: fetcher.Result{
		FetchClass: model.FetchOK,
		HTTPStatus: 200,
		Body:       []byte("<html><head><title>Deep</title></head><body>too deep</body></html>"),
		Header:     http.Header{},
	}}
	c.Extractor = &fakeExtractor{err: fmt.Errorf("extract: parse failed: %w", extract.ErrDOMTooDeep)}
	fg := &fakeGraph{}
	c.Graph = fg

	u := model.URL{ID: 4, SiteID: 1, URL: "https://e.com/deep", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "")
	if res.FetchClass != model.FetchOK {
		t.Fatalf("FetchClass = %q, want ok", res.FetchClass)
	}
	if fg.calls != 0 {
		t.Errorf("Graph.SyncPage must NOT be called when extraction fails, got %d calls", fg.calls)
	}
}

// TestCrawlOneNilGraphCrawlsGreen (criterion 3) proves a nil Graph (feature OFF —
// the scope-gate severability) makes CrawlOne a no-op for the graph: a normal
// FetchOK crawl succeeds with no panic. Run under -race this also guards the seam
// against a data race on the (unset) field.
func TestCrawlOneNilGraphCrawlsGreen(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(`<html><head><title>T</title></head><body><a href="/x">x</a></body></html>`))
	}))
	defer srv.Close()

	c, _, getSaved := newPipeline(t)
	// c.Graph is nil (unwired). No Discoverer either: the GetSite is skipped entirely.
	u := model.URL{ID: 5, SiteID: 1, URL: srv.URL + "/", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "") // must not panic
	if res.Err != nil {
		t.Fatalf("CrawlOne err = %v (nil Graph must crawl green)", res.Err)
	}
	if res.FetchClass != model.FetchOK {
		t.Fatalf("FetchClass = %q, want ok", res.FetchClass)
	}
	if _, ok := getSaved(5); !ok {
		t.Error("snapshot should still be saved with a nil Graph")
	}
}

// TestCrawlOneGraphErrorSurfaced proves a SyncPage error is surfaced on res.Err
// (so a graph-write failure is recorded, not swallowed) without aborting the crawl —
// the snapshot is already persisted and the schedule still advances. Mirrors the
// Discoverer error-propagation contract.
func TestCrawlOneGraphErrorSurfaced(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(`<html><head><title>T</title></head><body><a href="/x">x</a></body></html>`))
	}))
	defer srv.Close()

	c, fc, getSaved := newPipeline(t)
	boom := fmt.Errorf("sync boom")
	c.Graph = &fakeGraph{err: boom}

	u := model.URL{ID: 6, SiteID: 1, URL: srv.URL + "/", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "")
	if res.Err == nil {
		t.Fatal("a SyncPage error must be surfaced on res.Err")
	}
	// Crawl still completed: the snapshot was persisted and the schedule advanced.
	if _, ok := getSaved(6); !ok {
		t.Error("snapshot must still be saved even when SyncPage errors (crawl not aborted)")
	}
	if _, ok := fc.scheduledNextAt[6]; !ok {
		t.Error("schedule must still advance even when SyncPage errors")
	}
}
