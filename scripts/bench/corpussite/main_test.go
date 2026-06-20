package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/benchcorpus"
)

// ---------------------------------------------------------------------------
// requireLoopback — the security guard that keeps the benchmark origin off
// public interfaces. Both arms are exercised: forms it MUST accept and forms
// it MUST reject. Literal IPs are pure (no DNS); the "localhost" case touches
// the resolver path.
// ---------------------------------------------------------------------------

func TestRequireLoopbackAcceptsLoopbackLiterals(t *testing.T) {
	// Every value in the IPv4 127.0.0.0/8 block and the IPv6 ::1 loopback must
	// be accepted — the guard binds loopback only but is not restricted to the
	// canonical 127.0.0.1.
	accept := []string{
		"127.0.0.1",
		"127.0.0.5", // anywhere in 127.0.0.0/8 is loopback
		"127.255.255.254",
		"::1",
	}
	for _, host := range accept {
		host := host
		t.Run(host, func(t *testing.T) {
			if err := requireLoopback(host); err != nil {
				t.Fatalf("requireLoopback(%q) = %v; want nil (loopback must be accepted)", host, err)
			}
		})
	}
}

func TestRequireLoopbackRejectsNonLoopback(t *testing.T) {
	// These must each be refused so corpussite can never bind a routable or
	// all-interfaces address. A nil error here would be a real security hole.
	reject := []struct {
		name string
		host string
	}{
		{"empty_binds_all_interfaces", ""},
		{"unspecified_v4", "0.0.0.0"},
		{"routable_public", "8.8.8.8"},
		{"private_rfc1918", "10.0.0.1"},
		{"private_192", "192.168.1.1"},
		{"unspecified_v6", "::"},
	}
	for _, tc := range reject {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if err := requireLoopback(tc.host); err == nil {
				t.Fatalf("requireLoopback(%q) = nil; want an error (non-loopback must be rejected)", tc.host)
			}
		})
	}
}

func TestRequireLoopbackEmptyHostMessageMentionsAllInterfaces(t *testing.T) {
	// The empty-host branch is distinct from the parse/resolve branches; assert
	// it is taken by checking its specific guidance, not just that it errors.
	err := requireLoopback("")
	if err == nil {
		t.Fatal("requireLoopback(\"\") = nil; want error")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("empty-host error %q does not mention loopback", err.Error())
	}
}

func TestRequireLoopbackUnresolvableHost(t *testing.T) {
	// A non-literal host that cannot resolve hits the LookupIP error arm and
	// must be rejected (we cannot prove it is loopback).
	if err := requireLoopback("nonexistent.invalid."); err == nil {
		t.Fatal("requireLoopback(unresolvable) = nil; want a resolve error")
	}
}

func TestRequireLoopbackLocalhostResolves(t *testing.T) {
	// "localhost" is a non-literal host: the guard must resolve it and accept it
	// only when every address is loopback. Guard against exotic resolvers so the
	// test asserts the guard's decision, not the box's /etc/hosts.
	ips, err := net.LookupIP("localhost")
	if err != nil || len(ips) == 0 {
		t.Skipf("localhost did not resolve in this environment (err=%v); skipping resolver-accept assertion", err)
	}
	allLoopback := true
	for _, ip := range ips {
		if !ip.IsLoopback() {
			allLoopback = false
			break
		}
	}
	got := requireLoopback("localhost")
	if allLoopback {
		if got != nil {
			t.Fatalf("requireLoopback(\"localhost\") = %v; localhost resolves to loopback-only %v, want nil", got, ips)
		}
	} else {
		if got == nil {
			t.Fatalf("requireLoopback(\"localhost\") = nil; localhost resolves to non-loopback %v, want error", ips)
		}
	}
}

// ---------------------------------------------------------------------------
// newCorpusServer + handlePage — driven through the real mux via httptest.
// ---------------------------------------------------------------------------

// startCorpus boots an httptest server over the corpusServer mux for cfg.
func startCorpus(t *testing.T, cfg config) *httptest.Server {
	t.Helper()
	srv := newCorpusServer(cfg)
	ts := httptest.NewServer(srv.mux())
	t.Cleanup(ts.Close)
	return ts
}

func TestNewCorpusServerInitialised(t *testing.T) {
	s := newCorpusServer(config{pages: 10})
	if s == nil {
		t.Fatal("newCorpusServer returned nil")
	}
	if s.revisions == nil {
		t.Fatal("newCorpusServer left revisions map nil; revisionOf would still work but churnLoop writes would panic")
	}
	if s.cfg.pages != 10 {
		t.Fatalf("cfg not stored: pages = %d, want 10", s.cfg.pages)
	}
	// A freshly-built server has every page pristine (revision 0).
	if got := s.revisionOf(3); got != 0 {
		t.Fatalf("revisionOf(3) on fresh server = %d, want 0", got)
	}
}

func TestHandlePageServesCorpusBytes(t *testing.T) {
	ts := startCorpus(t, config{pages: 20})

	// A valid page of each class returns 200 + text/html + the EXACT pristine
	// benchcorpus bytes (no etag flag => no conditional handling).
	for _, tc := range []struct {
		class benchcorpus.Class
		index int
	}{
		{benchcorpus.Landing, 0},
		{benchcorpus.Article, 7},
		{benchcorpus.Listing, 12},
	} {
		path := benchcorpus.Path(tc.class, tc.index)
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
				t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", ct)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			want := benchcorpus.Page(tc.class, tc.index)
			if string(body) != string(want) {
				t.Fatalf("body for %s does not match benchcorpus.Page (got %d bytes, want %d)", path, len(body), len(want))
			}
			// Content-Length header must equal the served bytes.
			if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(want)) {
				t.Fatalf("Content-Length = %q, want %d", cl, len(want))
			}
		})
	}
}

func TestHandlePageRootIsPlainTextNotCorpus(t *testing.T) {
	ts := startCorpus(t, config{pages: 5})
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("root status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("root Content-Type = %q, want text/plain prefix", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "loopback only") {
		t.Fatalf("root body = %q, want the loopback-only index notice", string(body))
	}
}

func TestHandlePageNotFound(t *testing.T) {
	ts := startCorpus(t, config{pages: 5}) // valid indices: 0..4

	notFound := []struct {
		name string
		path string
	}{
		{"unknown_scheme", "/does/not/parse"},
		{"index_out_of_range_high", benchcorpus.Path(benchcorpus.Article, 5)}, // pages=5 => index 5 invalid
		{"index_far_out_of_range", benchcorpus.Path(benchcorpus.Landing, 9999)},
		{"unknown_class", "/widget/1"},
	}
	for _, tc := range notFound {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status for %s = %d, want 404", tc.path, resp.StatusCode)
			}
		})
	}
}

func TestHandlePageInRangeIsServed(t *testing.T) {
	// Boundary: with pages=5, index 4 is the last VALID page and must be 200.
	ts := startCorpus(t, config{pages: 5})
	path := benchcorpus.Path(benchcorpus.Landing, 4)
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("last in-range index status = %d, want 200", resp.StatusCode)
	}
}

func TestHandlePageHeadHasHeadersNoBody(t *testing.T) {
	// HEAD must carry the same metadata (Content-Length) but no body, per the
	// r.Method != HEAD guard.
	ts := startCorpus(t, config{pages: 5})
	path := benchcorpus.Path(benchcorpus.Landing, 0)
	req, err := http.NewRequest(http.MethodHead, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new HEAD request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Fatalf("HEAD returned a body of %d bytes; want empty", len(body))
	}
	want := len(benchcorpus.Page(benchcorpus.Landing, 0))
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(want) {
		t.Fatalf("HEAD Content-Length = %q, want %d", cl, want)
	}
}

// ---------------------------------------------------------------------------
// ETag / conditional-request behavior (drives handlePage with -etag) and the
// "changed page" path: a churned page differs in bytes and ETag.
// ---------------------------------------------------------------------------

func TestHandlePageEtagSetAndConditional304(t *testing.T) {
	ts := startCorpus(t, config{pages: 10, etag: true})
	path := benchcorpus.Path(benchcorpus.Article, 2)

	// First GET: 200 + an ETag header.
	resp1, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	etag := resp1.Header.Get("ETag")
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first GET status = %d, want 200", resp1.StatusCode)
	}
	if etag == "" {
		t.Fatal("ETag header missing with -etag enabled")
	}

	// Conditional GET with the matching If-None-Match => 304 and NO body.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional GET status = %d, want 304", resp2.StatusCode)
	}
	if len(body2) != 0 {
		t.Fatalf("304 carried a body of %d bytes; want empty", len(body2))
	}

	// A non-matching If-None-Match must NOT short-circuit: full 200 + body.
	req3, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req3.Header.Set("If-None-Match", `"some-other-etag"`)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("stale conditional GET: %v", err)
	}
	body3, _ := io.ReadAll(resp3.Body)
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("stale conditional GET status = %d, want 200", resp3.StatusCode)
	}
	if string(body3) != string(body1) {
		t.Fatal("stale conditional GET body differs from the unconditional GET body")
	}
}

func TestHandlePageNoEtagHeaderWhenDisabled(t *testing.T) {
	// With -etag off, no ETag is emitted and a conditional request still gets a
	// full 200 (the 304 path is gated behind cfg.etag).
	ts := startCorpus(t, config{pages: 10, etag: false})
	path := benchcorpus.Path(benchcorpus.Article, 2)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Header.Set("If-None-Match", `"article-2-r0"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET (etag off): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("etag-off conditional status = %d, want 200 (304 path must be gated off)", resp.StatusCode)
	}
	if et := resp.Header.Get("ETag"); et != "" {
		t.Fatalf("ETag = %q with -etag off; want none", et)
	}
}

func TestHandlePageChangedAfterChurnDiffersBytesAndEtag(t *testing.T) {
	// Drive the "changed" path directly: bump a page's revision the way
	// churnLoop does, then assert the served bytes AND ETag both change, while
	// the pristine bytes are still recoverable from benchcorpus.
	cfg := config{pages: 10, etag: true}
	srv := newCorpusServer(cfg)
	ts := httptest.NewServer(srv.mux())
	t.Cleanup(ts.Close)

	const index = 2 // article in the landing/article/listing cycle
	class := benchcorpus.ClassForIndex(index)
	path := benchcorpus.Path(class, index)

	get := func() (int, string, string) {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("ETag"), string(b)
	}

	status0, etag0, body0 := get()
	if status0 != http.StatusOK {
		t.Fatalf("pristine status = %d, want 200", status0)
	}
	if body0 != string(benchcorpus.Page(class, index)) {
		t.Fatal("pristine body != benchcorpus.Page")
	}

	// Simulate one churn epoch for this page (what churnLoop does under lock).
	srv.mu.Lock()
	srv.revisions[index]++
	srv.mu.Unlock()

	status1, etag1, body1 := get()
	if status1 != http.StatusOK {
		t.Fatalf("post-churn status = %d, want 200", status1)
	}
	if body1 == body0 {
		t.Fatal("post-churn body is identical to pristine; mutate did not change the bytes")
	}
	if etag1 == etag0 {
		t.Fatalf("post-churn ETag %q == pristine ETag %q; ETag must change with revision", etag1, etag0)
	}
	// The mutated body must be longer (title suffix + injected marker paragraph)
	// and contain the revision marker.
	if len(body1) <= len(body0) {
		t.Fatalf("mutated body (%d) not longer than pristine (%d)", len(body1), len(body0))
	}
	if !strings.Contains(body1, `data-rev="1"`) {
		t.Fatal("mutated body missing the revision-1 marker paragraph")
	}
	if !strings.Contains(body1, "(rev 1)") {
		t.Fatal("mutated body missing the (rev 1) title suffix")
	}

	// The old ETag, presented conditionally, must now MISS (page changed) => 200.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Header.Set("If-None-Match", etag0)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET with stale etag: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stale-etag conditional after churn = %d, want 200 (not 304)", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// handleWebhook — drains + discards the body and replies 200 without echoing.
// ---------------------------------------------------------------------------

func TestHandleWebhookDiscardsBodyAnd200s(t *testing.T) {
	ts := startCorpus(t, config{pages: 5})
	payload := "this is a fake alert payload that must be discarded, not echoed"
	resp, err := http.Post(ts.URL+"/webhook", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /webhook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/webhook status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /webhook response: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("/webhook echoed %d bytes; want empty (body must be discarded, never echoed)", len(body))
	}
	if strings.Contains(string(body), "alert payload") {
		t.Fatal("/webhook response echoed the request payload")
	}
}

// ---------------------------------------------------------------------------
// mutate + revisionOf + pageBytes — deterministic, revision-dependent bytes.
// ---------------------------------------------------------------------------

func TestMutateDeterministicAndRevisionDependent(t *testing.T) {
	const index = 1
	class := benchcorpus.ClassForIndex(index)
	base := benchcorpus.Page(class, index)

	// Same (page, rev) => identical bytes (reproducible churn, no rand).
	m1a := mutate(base, index, 1)
	m1b := mutate(base, index, 1)
	if string(m1a) != string(m1b) {
		t.Fatal("mutate is not deterministic: two calls with rev=1 differ")
	}

	// Different revisions => different bytes.
	m2 := mutate(base, index, 2)
	if string(m1a) == string(m2) {
		t.Fatal("mutate(rev=1) == mutate(rev=2); revision must change the bytes")
	}

	// mutate must not modify the input slice (builds a fresh slice).
	beforeLen := len(base)
	beforeCopy := string(base)
	_ = mutate(base, index, 3)
	if len(base) != beforeLen || string(base) != beforeCopy {
		t.Fatal("mutate mutated its input page slice")
	}

	// The mutated bytes encode the revision in both the title and the marker.
	got := string(m2)
	if !strings.Contains(got, "(rev 2)") {
		t.Fatal("mutate(rev=2) missing the (rev 2) title suffix")
	}
	if !strings.Contains(got, `data-rev="2"`) {
		t.Fatal("mutate(rev=2) missing the data-rev=\"2\" marker")
	}
	// Marker references the page's own path.
	if !strings.Contains(got, benchcorpus.Path(benchcorpus.ClassForIndex(index), index)) {
		t.Fatal("mutate marker does not reference the page path")
	}
}

func TestMutateNoMainAppendsMarker(t *testing.T) {
	// Defensive branch: a page without </main> gets the marker appended at the
	// end rather than injected. (Real corpus pages have </main>; this proves the
	// else branch.) Also covers the no-</title> case (no panic, no suffix).
	base := []byte("<html><body>no main here</body></html>")
	got := string(mutate(base, 0, 4))
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), `for `+benchcorpus.Path(benchcorpus.ClassForIndex(0), 0)+`.</p>`) {
		t.Fatalf("expected marker appended at end, got tail %q", got)
	}
	if !strings.Contains(got, `data-rev="4"`) {
		t.Fatal("appended marker missing data-rev=\"4\"")
	}
	// No </title> => the title suffix must NOT appear.
	if strings.Contains(got, "(rev 4)") {
		t.Fatal("title suffix injected even though there is no </title>")
	}
}

func TestPageBytesRevisionZeroIsPristine(t *testing.T) {
	const index = 0
	class := benchcorpus.ClassForIndex(index)
	pristine := benchcorpus.Page(class, index)

	// rev 0 returns the pristine page verbatim.
	if got := pageBytes(class, index, 0); string(got) != string(pristine) {
		t.Fatal("pageBytes(rev=0) != benchcorpus.Page")
	}
	// rev > 0 returns the mutated variant (== mutate of the pristine page).
	want := mutate(pristine, index, 5)
	if got := pageBytes(class, index, 5); string(got) != string(want) {
		t.Fatal("pageBytes(rev=5) != mutate(page, index, 5)")
	}
}

func TestRevisionOfDefaultsZeroReflectsBump(t *testing.T) {
	s := newCorpusServer(config{pages: 10})
	if got := s.revisionOf(4); got != 0 {
		t.Fatalf("revisionOf(4) on fresh server = %d, want 0", got)
	}
	s.mu.Lock()
	s.revisions[4] = 3
	s.mu.Unlock()
	if got := s.revisionOf(4); got != 3 {
		t.Fatalf("revisionOf(4) after set = %d, want 3", got)
	}
	// Untouched indices stay 0.
	if got := s.revisionOf(5); got != 0 {
		t.Fatalf("revisionOf(5) = %d, want 0 (untouched)", got)
	}
}

// ---------------------------------------------------------------------------
// etagFor / etagMatches
// ---------------------------------------------------------------------------

func TestEtagForStableAndRevisionScoped(t *testing.T) {
	// Stable for identical coordinates (call twice; must agree).
	first := etagFor(benchcorpus.Article, 7, 0)
	second := etagFor(benchcorpus.Article, 7, 0)
	if first != second {
		t.Fatalf("etagFor is not stable for identical coordinates: %q vs %q", first, second)
	}
	// Encodes class, index, revision; each coordinate participates.
	cases := []struct {
		a, b string
		why  string
	}{
		{etagFor(benchcorpus.Article, 7, 0), etagFor(benchcorpus.Article, 7, 1), "revision must change the etag"},
		{etagFor(benchcorpus.Article, 7, 0), etagFor(benchcorpus.Article, 8, 0), "index must change the etag"},
		{etagFor(benchcorpus.Article, 7, 0), etagFor(benchcorpus.Landing, 7, 0), "class must change the etag"},
	}
	for _, c := range cases {
		if c.a == c.b {
			t.Fatalf("%s: both = %q", c.why, c.a)
		}
	}
	// Shape: quoted strong validator embedding the coordinates.
	et := etagFor(benchcorpus.Article, 7, 3)
	if !strings.HasPrefix(et, `"`) || !strings.HasSuffix(et, `"`) {
		t.Fatalf("etag %q is not a quoted strong validator", et)
	}
	if et != `"article-7-r3"` {
		t.Fatalf("etag = %q, want \"article-7-r3\"", et)
	}
}

func TestEtagMatches(t *testing.T) {
	const etag = `"article-7-r3"`
	tests := []struct {
		name        string
		ifNoneMatch string
		want        bool
	}{
		{"exact", `"article-7-r3"`, true},
		{"wildcard", "*", true},
		{"leading_space", `  "article-7-r3"`, true},
		{"trailing_space", `"article-7-r3"   `, true},
		{"weak_prefix", `W/"article-7-r3"`, true},
		{"comma_list_first", `"article-7-r3", "other"`, true},
		{"comma_list_second", `"other", "article-7-r3"`, true},
		{"comma_list_weak_match", `"x", W/"article-7-r3"`, true},
		{"no_match", `"article-7-r9"`, false},
		{"empty", "", false},
		{"different_index", `"article-8-r3"`, false},
		{"substring_not_token", `"article-7-r3-extra"`, false},
		{"comma_list_no_match", `"a", "b", "c"`, false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := etagMatches(tc.ifNoneMatch, etag); got != tc.want {
				t.Fatalf("etagMatches(%q, %q) = %v, want %v", tc.ifNoneMatch, etag, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseFlags
// ---------------------------------------------------------------------------

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want config
	}{
		{
			name: "defaults",
			args: nil,
			want: config{addr: "127.0.0.1:0", pages: 500, churnPct: 0, churnEvery: time.Minute, etag: false},
		},
		{
			name: "all_set",
			args: []string{"-addr", "::1:0", "-pages", "42", "-churn-pct", "25", "-churn-every", "5s", "-etag"},
			want: config{addr: "::1:0", pages: 42, churnPct: 25, churnEvery: 5 * time.Second, etag: true},
		},
		{
			name: "etag_explicit_false",
			args: []string{"-etag=false", "-pages", "1"},
			want: config{addr: "127.0.0.1:0", pages: 1, churnPct: 0, churnEvery: time.Minute, etag: false},
		},
		{
			name: "equals_form",
			args: []string{"-addr=127.0.0.1:9999", "-churn-pct=100", "-churn-every=250ms"},
			want: config{addr: "127.0.0.1:9999", pages: 500, churnPct: 100, churnEvery: 250 * time.Millisecond, etag: false},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := parseFlags(tc.args, io.Discard)
			if got != tc.want {
				t.Fatalf("parseFlags(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// small string helpers — table-driven, including edge cases.
// ---------------------------------------------------------------------------

func TestIndexOf(t *testing.T) {
	tests := []struct {
		name string
		s    string
		sub  string
		want int
	}{
		{"found_middle", "hello world", "o w", 4},
		{"found_start", "abcdef", "abc", 0},
		{"found_end", "abcdef", "def", 3},
		{"no_match", "abcdef", "xyz", -1},
		{"empty_sub_returns_0", "abc", "", 0},
		{"empty_sub_empty_s", "", "", 0},
		{"sub_longer_than_s", "ab", "abc", -1},
		{"empty_s_nonempty_sub", "", "a", -1},
		{"first_occurrence_only", "aXaXa", "aX", 0},
		{"single_char", "abc", "b", 1},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := indexOf(tc.s, tc.sub); got != tc.want {
				t.Fatalf("indexOf(%q, %q) = %d, want %d", tc.s, tc.sub, got, tc.want)
			}
		})
	}
}

func TestTrimSpace(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"none", "abc", "abc"},
		{"leading_spaces", "   abc", "abc"},
		{"trailing_spaces", "abc   ", "abc"},
		{"both", "  abc  ", "abc"},
		{"tabs", "\t\tabc\t", "abc"},
		{"mixed_space_tab", " \t abc \t ", "abc"},
		{"all_space", "    ", ""},
		{"empty", "", ""},
		{"inner_space_preserved", "  a b c  ", "a b c"},
		{"newline_not_trimmed", " \nabc\n ", "\nabc\n"}, // only space+tab are trimmed
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := trimSpace(tc.in); got != tc.want {
				t.Fatalf("trimSpace(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTrimPrefix(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		prefix string
		want   string
	}{
		{"has_prefix", "W/etag", "W/", "etag"},
		{"no_prefix", "etag", "W/", "etag"},
		{"empty_prefix", "etag", "", "etag"},
		{"prefix_equals_s", "W/", "W/", ""},
		{"prefix_longer_than_s", "W", "W/", "W"},
		{"empty_s", "", "W/", ""},
		{"only_strips_once", "W/W/etag", "W/", "W/etag"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := trimPrefix(tc.s, tc.prefix); got != tc.want {
				t.Fatalf("trimPrefix(%q, %q) = %q, want %q", tc.s, tc.prefix, got, tc.want)
			}
		})
	}
}

func TestSplitComma(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"single", "a", []string{"a"}},
		{"two", "a,b", []string{"a", "b"}},
		{"three", "a,b,c", []string{"a", "b", "c"}},
		{"empty_string_one_empty_element", "", []string{""}},
		{"trailing_comma", "a,", []string{"a", ""}},
		{"leading_comma", ",a", []string{"", "a"}},
		{"consecutive_commas", "a,,b", []string{"a", "", "b"}},
		{"spaces_preserved", "a , b", []string{"a ", " b"}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := splitComma(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("splitComma(%q) = %#v (len %d), want %#v (len %d)", tc.in, got, len(got), tc.want, len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("splitComma(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}
