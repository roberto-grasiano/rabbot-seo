package precheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

const testUA = "Rabbot-SEO/test (+https://example.test)"

// newEgress returns an httptest server that echoes a fixed egress IP.
func newEgress(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("198.51.100.7"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunServerRenderedGreen(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Real SSR Page</title>` +
			`<meta name="description" content="desc"></head><body><h1>Hi</h1>` +
			`<p>Plenty of genuine server rendered words here so this is clearly ` +
			`a server rendered page for the precheck detector heuristics.</p></body></html>`))
	}))
	defer site.Close()
	egress := newEgress(t)

	rep, err := Run(context.Background(), site.URL, Options{
		UserAgent:      testUA,
		EgressEndpoint: egress.URL,
		AllowPrivate:   true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if rep.Verdict != VerdictGreen {
		t.Errorf("Verdict = %q, want green", rep.Verdict)
	}
	if rep.JS.Kind != ServerRendered {
		t.Errorf("JS.Kind = %q, want server_rendered", rep.JS.Kind)
	}
	if !rep.JS.ContentVisibleToCrawler {
		t.Errorf("ContentVisibleToCrawler = false, want true")
	}
	// Report.Doctor must be the reused fetcher.DoctorReport, populated verbatim.
	if rep.Doctor.HomepageStatus != 200 {
		t.Errorf("Doctor.HomepageStatus = %d, want 200", rep.Doctor.HomepageStatus)
	}
	if rep.Doctor.RobotsVerdict != "allowed" {
		t.Errorf("Doctor.RobotsVerdict = %q, want allowed", rep.Doctor.RobotsVerdict)
	}
	if strings.TrimSpace(rep.Summary) == "" {
		t.Errorf("Report.Summary empty")
	}
}

func TestRunBlockedRed(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(403)
		_, _ = w.Write([]byte("Attention Required! | Cloudflare"))
	}))
	defer site.Close()
	egress := newEgress(t)

	rep, err := Run(context.Background(), site.URL, Options{
		UserAgent: testUA, EgressEndpoint: egress.URL, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if rep.Verdict != VerdictRed {
		t.Errorf("Verdict = %q, want red", rep.Verdict)
	}
	if !rep.Doctor.Blocked {
		t.Errorf("Doctor.Blocked = false, want true")
	}
	// No body on a non-ok fetch => detector returns Unknown.
	if rep.JS.Kind != Unknown {
		t.Errorf("JS.Kind = %q, want unknown (no body on blocked fetch)", rep.JS.Kind)
	}
}

// TestRunSoftBlockRed proves a soft block (429/503) also grades RED: fetcher.Doctor
// sets Blocked=true for soft blocks too, and grade() reds out on dr.Blocked. This guards
// the soft-block path against a future refactor of the Blocked condition (the hard-block
// path is covered by TestRunBlockedRed).
func TestRunSoftBlockRed(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
		_, _ = w.Write([]byte("rate limited — try again later"))
	}))
	defer site.Close()
	egress := newEgress(t)

	rep, err := Run(context.Background(), site.URL, Options{
		UserAgent: testUA, EgressEndpoint: egress.URL, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if rep.Doctor.FetchClass != model.FetchSoftBlock {
		t.Fatalf("FetchClass = %q, want soft_block", rep.Doctor.FetchClass)
	}
	if !rep.Doctor.Blocked {
		t.Errorf("Doctor.Blocked = false, want true (soft block)")
	}
	if rep.Verdict != VerdictRed {
		t.Errorf("Verdict = %q, want red (soft block)", rep.Verdict)
	}
}

func TestRunRobotsDisallowedRed(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>fine</title></head><body><p>content content content content content content</p></body></html>`))
	}))
	defer site.Close()
	egress := newEgress(t)

	rep, err := Run(context.Background(), site.URL, Options{
		UserAgent: testUA, EgressEndpoint: egress.URL, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if rep.Doctor.RobotsVerdict != "disallowed" {
		t.Fatalf("RobotsVerdict = %q, want disallowed", rep.Doctor.RobotsVerdict)
	}
	if rep.Verdict != VerdictRed {
		t.Errorf("Verdict = %q, want red (robots disallow overrides a fine body)", rep.Verdict)
	}
}

func TestRunClientShellRed(t *testing.T) {
	shell := `<html><head></head><body><div id="root"></div><script>` +
		strings.Repeat("var x=1;/*pad*/", 2000) + `</script></body></html>`
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(shell))
	}))
	defer site.Close()
	egress := newEgress(t)

	rep, err := Run(context.Background(), site.URL, Options{
		UserAgent: testUA, EgressEndpoint: egress.URL, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if rep.JS.Kind != ClientShell {
		t.Fatalf("JS.Kind = %q, want client_shell", rep.JS.Kind)
	}
	if rep.Verdict != VerdictRed {
		t.Errorf("Verdict = %q, want red (client shell hint)", rep.Verdict)
	}
	if rep.JS.ContentVisibleToCrawler {
		t.Errorf("ContentVisibleToCrawler = true, want false")
	}
}

func TestRunNextDataGreen(t *testing.T) {
	// The payload recovers a head field (title), so it grades Hydrated/recoverable → green.
	// A structural-only payload that recovered nothing would NOT (the field-recovery gate).
	page := `<html><head></head><body><div id="__next"></div>` +
		`<script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"title":"Hydrated Title"}},"page":"/"}</script>` +
		`<script src="/_next/static/main.js"></script></body></html>`
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(page))
	}))
	defer site.Close()
	egress := newEgress(t)

	rep, err := Run(context.Background(), site.URL, Options{
		UserAgent: testUA, EgressEndpoint: egress.URL, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if rep.JS.Kind != Hydrated {
		t.Errorf("JS.Kind = %q, want hydrated", rep.JS.Kind)
	}
	if rep.Verdict != VerdictGreen {
		t.Errorf("Verdict = %q, want green (hydration payload recoverable)", rep.Verdict)
	}
}

// TestRunHeadOnlyShellYellow proves the honest "partial" verdict: a page whose head is
// server-rendered but whose body is an empty client shell (no hydration payload) grades
// YELLOW — neither a falsely-green "fully monitorable" nor a red "blocked".
func TestRunHeadOnlyShellYellow(t *testing.T) {
	page := `<html><head><title>App</title><meta name="description" content="d"></head>` +
		`<body><div id="root"></div><script src="/assets/index.js"></script></body></html>`
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(page))
	}))
	defer site.Close()
	egress := newEgress(t)

	rep, err := Run(context.Background(), site.URL, Options{
		UserAgent: testUA, EgressEndpoint: egress.URL, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if rep.JS.Kind != HeadOnlyShell {
		t.Fatalf("JS.Kind = %q, want head_only_shell", rep.JS.Kind)
	}
	if rep.Verdict != VerdictYellow {
		t.Errorf("Verdict = %q, want yellow (head monitorable, body client-rendered)", rep.Verdict)
	}
	if rep.JS.ContentVisibleToCrawler {
		t.Errorf("ContentVisibleToCrawler = true, want false")
	}
}

// TestRunUnknownYellow proves a reachable, allowed, but non-confident page (here a
// non-HTML 200 with no body retained as HTML) grades yellow, not red or green.
func TestRunUnknownYellow(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer site.Close()
	egress := newEgress(t)

	rep, err := Run(context.Background(), site.URL, Options{
		UserAgent: testUA, EgressEndpoint: egress.URL, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if rep.Doctor.FetchClass != model.FetchOK {
		t.Fatalf("FetchClass = %q, want ok (200 fetch)", rep.Doctor.FetchClass)
	}
	if rep.JS.Kind != Unknown {
		t.Errorf("JS.Kind = %q, want unknown (non-HTML body)", rep.JS.Kind)
	}
	if rep.Verdict != VerdictYellow {
		t.Errorf("Verdict = %q, want yellow", rep.Verdict)
	}
}
