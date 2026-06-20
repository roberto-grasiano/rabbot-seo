package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/diff"
	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/notify"
	"github.com/roberto-grasiano/rabbot-seo/internal/rules"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// heavyBundle is an inline <script> body larger than precheck's scriptCeil (4096 bytes)
// of *executable* JS — NOT a recoverable-data payload (no id="__NEXT_DATA__", no
// type="application/ld+json"), so precheck counts it toward ScriptBytes. The client_shell
// verdict (detect.go) requires ScriptBytes > scriptCeil, so the bundle must clear 4096
// bytes; we pad well past it so the verdict is robust to threshold tweaks.
var heavyBundle = "var __app=function(){" + strings.Repeat("a=a+1;", 1000) + "};__app();"

// The three fixtures below are deliberately constructed so the ONLY field that differs
// between a "full" page and its matching "shell" is the body prose (which intrinsically
// drives the render_mode flip AND a legitimate `content` change — a body that empties is a
// real content change, never a render_mode double-fire). Every test asserts on render_mode
// PRECISELY (exactly one render_mode/needs_rendering event, exactly one issue) so the
// "exactly one alert" proof is about the rules_bridge dedup, not about suppressing the
// unavoidable collateral `content` signal.

// clientShellPage: empty framework root (#__next, zero visible words), thin body, a large
// *executable* inline bundle, NO head fields (no title/meta/h1), NO hydration payload — the
// client_shell conjunction in precheck.DetectDoc (emptyRoot && words<floor && scriptBytes>ceil
// && !hasHeadFields && no payload). Content is unrecoverable without JS — "not monitored
// beyond fetch status".
func clientShellPage() string {
	return `<html><head></head><body><div id="__next"></div>` +
		`<script>` + heavyBundle + `</script></body></html>`
}

// noHeadServerRenderedPage is the client_shell test's BASELINE: NO head fields (matching
// clientShellPage so title/meta/h1 do not change across the flip) but real body prose well
// above the thinness floor, so it classifies server_rendered (words >= floor). The flip to
// clientShellPage therefore changes only `content` (prose -> empty) and `render_mode` — no
// title/meta/headings collateral.
func noHeadServerRenderedPage() string {
	return `<html><head></head><body>` +
		`<p>This baseline page carries plenty of real visible prose words rendered directly ` +
		`into the server HTML so the crawler sees substantial content far above the thinness ` +
		`floor and classifies it as server rendered without needing any JavaScript at all.</p>` +
		`</body></html>`
}

// headedServerRenderedPage is the head_only_shell test's BASELINE: the SAME head (title +
// meta + canonical) as headOnlyShellPage, plus body prose above the floor (server_rendered).
// The flip to headOnlyShellPage keeps the head identical, so only `content` and `render_mode`
// change — no title/meta collateral.
func headedServerRenderedPage(canonical string) string {
	return `<html><head><title>Shell App</title>` +
		`<meta name="description" content="a head-only shell page">` +
		`<link rel="canonical" href="` + canonical + `">` +
		`</head><body>` +
		`<p>This baseline page carries plenty of real visible prose words rendered directly ` +
		`into the server HTML so the crawler sees substantial content far above the thinness ` +
		`floor and classifies it as server rendered without needing any JavaScript at all.</p>` +
		`</body></html>`
}

// headOnlyShellPage: server-rendered SEO head (title + meta), but an empty framework root
// with a thin body and no hydration payload — the head_only_shell verdict (hasHeadFields &&
// emptyRoot && words<floor && !nextFlightPresent). Head monitored; body not. Head is byte-
// identical to headedServerRenderedPage so the flip touches only body content + render_mode.
func headOnlyShellPage(canonical string) string {
	return `<html><head><title>Shell App</title>` +
		`<meta name="description" content="a head-only shell page">` +
		`<link rel="canonical" href="` + canonical + `">` +
		`</head><body><div id="__next"></div></body></html>`
}

// needsRenderingHarness wires the REAL crawl pipeline (CrawlOne: robots -> fetch ->
// extract[+A8 classify] -> persist -> M2 process -> rules -> alert pipeline -> mock Slack)
// against an httptest origin whose served body is chosen by the `body` accessor, mirroring
// rich_result_e2e_test.go. It returns the pieces a test drives.
type needsRenderingHarness struct {
	crawlNow  func(phase string)
	deps      *recordingDeps
	slackHits *int32
	st        *store.DB
	urlID     int64
	advance   func(d time.Duration)
}

func newNeedsRenderingHarness(t *testing.T, body func() string) *needsRenderingHarness {
	t.Helper()
	ctx := context.Background()

	// Mutable fake clock, advanced between phases so per-incident dedup never silently
	// swallows a later phase's notification inside the DedupWindow.
	var clockMu sync.Mutex
	cur := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return cur
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		cur = cur.Add(d)
		clockMu.Unlock()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body()))
	})
	origin := httptest.NewServer(mux)
	t.Cleanup(origin.Close)

	var slackHits int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&slackHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slackSrv.Close)

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	siteID, err := st.AddSite(ctx, model.Site{
		BaseURL: origin.URL, Name: "Origin", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	urlID, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: origin.URL, FirstSeen: clock(), NextCheckAt: clock(), Interval: 600, Importance: 1.0,
	})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}

	notifier := notify.NewSlackNotifier("slack-critical", slackSrv.URL, slackSrv.Client())
	registry := notify.NewRegistry(
		map[string]notify.Notifier{"slack-critical": notifier},
		// Match-all route: needs_rendering is a WARNING, so the route must admit warnings
		// (a critical-only route would silently drop the proof).
		[]config.RouteConfig{{Match: map[string]string{}, Notifier: "slack-critical"}},
	)
	pipeline := alerts.NewPipeline(st, notify.NewDispatcher(registry),
		alerts.WithCaps(alerts.Caps{DedupWindow: 5 * time.Minute, HourlyCap: 30, IncidentAutoClose: 24 * time.Hour}),
		alerts.WithClock(clock),
	)
	engine := rules.NewEngine(rules.DefaultRuleSet(), st, clock)
	deps := &recordingDeps{inner: &e2eDeps{store: st, engine: engine, pipeline: pipeline}}
	proc := NewProcessor(deps, diff.DefaultSimhashThreshold, clock)

	crawler := &Crawler{
		Store:     st,
		Fetcher:   fetcher.New(fetcher.Options{UserAgent: "Rabbot-SEO/test", Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true}),
		Extractor: extract.NewExtractor(),
		Robots:    frontier.NewRobotsCache(origin.Client(), "Rabbot-SEO/test", time.Minute),
		Frontier:  frontier.New(frontier.Options{PerHostRate: time.Millisecond, PerHostConcurrency: 4}),
		Now:       clock,
		Processor: proc,
		// A8 recovery enabled — exercise the full hydration seam. render_mode is classified
		// regardless of this knob, but enabling it proves the real production path.
		Hydration: extract.HydrationOptions{Enabled: true, MaxPayloadBytes: 2 << 20},
	}

	crawlNow := func(phase string) {
		u, err := st.GetURL(ctx, siteID, origin.URL)
		if err != nil {
			t.Fatalf("GetURL (%s): %v", phase, err)
		}
		if r := crawler.CrawlOne(ctx, u, 600, 86400, ""); r.Err != nil {
			t.Fatalf("CrawlOne (%s): %v", phase, r.Err)
		}
	}

	return &needsRenderingHarness{
		crawlNow:  crawlNow,
		deps:      deps,
		slackHits: &slackHits,
		st:        st,
		urlID:     urlID,
		advance:   advance,
	}
}

// TestNeedsRenderingClientShellAlertsAndRecoversE2E is the A8 scheduler END-TO-END proof
// (acceptance criteria 6 + 7, the e2e half), driven through the REAL crawl pipeline,
// mirroring rich_result_e2e_test.go.
//
//   - BASELINE: a server_rendered page (no head, but real prose). render_mode=server_rendered,
//     no needs_rendering issue, NO alert. Establishing a prior baseline is essential: a
//     brand-new client_shell URL would be SUPPRESSED by ProcessFetch's first-crawl bridge
//     guard, so the alert proof MUST be a transition INTO client_shell (durable BOTH-ARMS lesson).
//   - REGRESSION: the origin redeploys as a client_shell. render_mode flips to client_shell and
//     PERSISTS (asserted on the scanned-back snapshot — persisted-encoding lesson); the
//     needs_rendering rule opens EXACTLY ONE warning Issue whose detail says content is not
//     monitored beyond fetch status; EXACTLY ONE render_mode alert event is ingested (no
//     rule-fire + render_mode change-event double-fire — the rules_bridge dedup holds e2e).
//   - RECOVERY: the origin redeploys back to server_rendered. render_mode recovers, the rule
//     passes, the Issue CLOSES via the engine lifecycle, and NO further render_mode alert fires.
func TestNeedsRenderingClientShellAlertsAndRecoversE2E(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mode atomic.Value // "server" | "client_shell"
	mode.Store("server")
	h := newNeedsRenderingHarness(t, func() string {
		if mode.Load().(string) == "client_shell" {
			return clientShellPage()
		}
		return noHeadServerRenderedPage()
	})

	// ── BASELINE: server_rendered, no issue, no alert ───────────────────────
	h.crawlNow("baseline")
	if got := atomic.LoadInt32(h.slackHits); got != 0 {
		t.Fatalf("server-rendered baseline must not alert; slackHits=%d", got)
	}
	snap1, err := h.st.LatestSnapshot(ctx, h.urlID)
	if err != nil {
		t.Fatalf("LatestSnapshot baseline: %v", err)
	}
	if snap1.RenderMode != model.RenderServerRendered {
		t.Fatalf("baseline render_mode = %q, want %q", snap1.RenderMode, model.RenderServerRendered)
	}
	if openHas(t, h.st, h.urlID, "needs_rendering") {
		t.Fatalf("server-rendered baseline must NOT open a needs_rendering issue")
	}

	// ── REGRESSION: flip to client_shell ────────────────────────────────────
	mode.Store("client_shell")
	h.advance(time.Hour) // past the DedupWindow: a real recheck interval later

	before := len(h.deps.ingestedEvents())
	h.crawlNow("client-shell")

	// render_mode persisted as client_shell (the value LatestSnapshot SCANNED BACK).
	snap2, err := h.st.LatestSnapshot(ctx, h.urlID)
	if err != nil {
		t.Fatalf("LatestSnapshot client-shell: %v", err)
	}
	if snap2.RenderMode != model.RenderClientShell {
		t.Fatalf("client-shell crawl render_mode = %q, want %q (the fixture must classify as client_shell)", snap2.RenderMode, model.RenderClientShell)
	}

	// EXACTLY ONE render_mode alert event this phase. render_mode is the bridged change_type
	// for needs_rendering AND the (skipped) standalone change field — a double-fire would show
	// up as TWO render_mode events. A `content` event is expected (the body genuinely emptied)
	// and is NOT a render_mode double-fire, so it is tolerated; any OTHER field would mean the
	// fixture leaked an unintended delta.
	phase2 := h.deps.ingestedEvents()[before:]
	renderEvents := countRenderModeEvents(t, phase2, "client_shell", "fetch_status_only")
	if renderEvents != 1 {
		t.Fatalf("client-shell regression must ingest EXACTLY ONE render_mode alert event (no rule/change double-fire), got %d (phase2 events %+v)", renderEvents, phase2)
	}
	assertOnlyExpectedFields(t, phase2, map[string]bool{"render_mode": true, "content": true})

	// EXACTLY ONE open needs_rendering issue, warning-tier, detail = not monitored beyond fetch.
	openIss := openIssues(t, h.st, h.urlID, "needs_rendering")
	if len(openIss) != 1 {
		t.Fatalf("client-shell must open EXACTLY ONE needs_rendering issue, got %d: %+v", len(openIss), openIss)
	}
	if openIss[0].Severity != model.SeverityWarning {
		t.Errorf("needs_rendering issue severity = %q, want warning", openIss[0].Severity)
	}
	if !strings.Contains(openIss[0].Detail, "fetch_status_only") {
		t.Errorf("client_shell issue detail must distinguish 'not monitored beyond fetch status' (fetch_status_only); got %q", openIss[0].Detail)
	}

	// ── RECOVERY: flip back to server_rendered ──────────────────────────────
	mode.Store("server")
	h.advance(time.Hour)

	ingestedBeforeRecovery := len(h.deps.ingestedEvents())
	resolvedBeforeRecovery := len(h.deps.resolvedEvents())
	h.crawlNow("recovery")

	snap3, err := h.st.LatestSnapshot(ctx, h.urlID)
	if err != nil {
		t.Fatalf("LatestSnapshot recovery: %v", err)
	}
	if snap3.RenderMode != model.RenderServerRendered {
		t.Fatalf("recovery render_mode = %q, want %q", snap3.RenderMode, model.RenderServerRendered)
	}
	// Recovery is a CLOSE, not a new finding: no NEW render_mode alert event is ingested.
	for _, e := range h.deps.ingestedEvents()[ingestedBeforeRecovery:] {
		if e.ChangeType == "render_mode" {
			t.Errorf("recovery crawl must not ingest a new render_mode alert event; got %+v", e)
		}
	}
	// The issue is CLOSED via the engine lifecycle (BOTH ARMS: opened in phase 2, closed here),
	// and recorded as closed (not merely absent) so the lifecycle is observable.
	if openHas(t, h.st, h.urlID, "needs_rendering") {
		t.Fatalf("recovery to server_rendered must CLOSE the needs_rendering issue (engine lifecycle); it is still open")
	}
	if !closedHas(t, h.st, h.urlID, "needs_rendering") {
		t.Errorf("the recovered needs_rendering issue must be recorded as closed")
	}
	// PUSH-SURFACE PARITY (PR #78 re-review): closing the issue ROW is not enough —
	// the bridged render_mode alert INCIDENT must resolve on recovery, not linger
	// until the 24h auto-close sweep. resolveHealthyFields emits exactly one
	// render_mode/warning resolve when the page leaves the shell states; its severity
	// must match the open incident's (warning) or the group fingerprint won't line up.
	renderResolves := 0
	for _, e := range h.deps.resolvedEvents()[resolvedBeforeRecovery:] {
		if e.ChangeType != "render_mode" {
			continue
		}
		renderResolves++
		if e.Severity != model.SeverityWarning {
			t.Errorf("render_mode resolve severity = %q, want warning (must match the open incident fingerprint)", e.Severity)
		}
	}
	if renderResolves != 1 {
		t.Errorf("recovery must emit EXACTLY ONE render_mode resolve so the needs_rendering alert closes promptly (not after the 24h sweep), got %d", renderResolves)
	}
}

// TestNeedsRenderingHeadOnlyShellDetailE2E proves the head_only_shell arm of acceptance
// criterion 6: a head_only_shell page opens a needs_rendering warning Issue whose detail
// distinguishes "head monitored; body not" (head_only) from the client_shell case
// ("fetch_status_only"). The baseline shares the head_only_shell page's exact head, so the
// flip touches only body content + render_mode (no title/meta collateral). Baseline is
// server_rendered so the transition INTO head_only_shell pages (not a suppressed first crawl).
func TestNeedsRenderingHeadOnlyShellDetailE2E(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mode atomic.Value // "server" | "head_only_shell"
	mode.Store("server")
	// canonical is cosmetic to the verdict; an empty <link> href is harmless. Keep both
	// pages' canonical identical (empty) so it never registers as a change.
	const canonical = ""
	h := newNeedsRenderingHarness(t, func() string {
		if mode.Load().(string) == "head_only_shell" {
			return headOnlyShellPage(canonical)
		}
		return headedServerRenderedPage(canonical)
	})

	// Baseline: server_rendered, no issue.
	h.crawlNow("baseline")
	if openHas(t, h.st, h.urlID, "needs_rendering") {
		t.Fatalf("server-rendered baseline must NOT open a needs_rendering issue")
	}

	// Flip to head_only_shell.
	mode.Store("head_only_shell")
	h.advance(time.Hour)
	before := len(h.deps.ingestedEvents())
	h.crawlNow("head-only-shell")

	snap, err := h.st.LatestSnapshot(ctx, h.urlID)
	if err != nil {
		t.Fatalf("LatestSnapshot head-only-shell: %v", err)
	}
	if snap.RenderMode != model.RenderHeadOnlyShell {
		t.Fatalf("head-only-shell crawl render_mode = %q, want %q (the fixture must classify as head_only_shell)", snap.RenderMode, model.RenderHeadOnlyShell)
	}

	// Exactly one bridged render_mode event, and its detail says head_only (NOT fetch_status_only).
	phase := h.deps.ingestedEvents()[before:]
	renderEvents := countRenderModeEvents(t, phase, "head_only_shell", "head_only")
	if renderEvents != 1 {
		t.Fatalf("head_only_shell must ingest EXACTLY ONE render_mode alert event, got %d (events %+v)", renderEvents, phase)
	}
	// The head is byte-identical across the flip, so only content + render_mode may change.
	assertOnlyExpectedFields(t, phase, map[string]bool{"render_mode": true, "content": true})

	openIss := openIssues(t, h.st, h.urlID, "needs_rendering")
	if len(openIss) != 1 {
		t.Fatalf("head_only_shell must open EXACTLY ONE needs_rendering issue, got %d: %+v", len(openIss), openIss)
	}
	// The DISTINCT detail: head monitored (head_only), NOT the client_shell phrasing.
	if !strings.Contains(openIss[0].Detail, "head_only") || strings.Contains(openIss[0].Detail, "fetch_status_only") {
		t.Errorf("head_only_shell issue detail must say head monitored / body not (head_only) and NOT the client_shell 'fetch_status_only' phrasing; got %q", openIss[0].Detail)
	}
}

// TestNeedsRenderingNormalPageNeverOpensIssueE2E is the negative arm (acceptance #6): a
// never-hydrated, normally server-rendered page crawled repeatedly NEVER opens a
// needs_rendering issue and NEVER fires a render_mode alert — server_rendered is a steady
// state, not a finding.
func TestNeedsRenderingNormalPageNeverOpensIssueE2E(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newNeedsRenderingHarness(t, func() string { return headedServerRenderedPage("") })

	// Crawl twice; nothing changes, so render_mode stays server_rendered throughout.
	h.crawlNow("first")
	h.advance(time.Hour)
	h.crawlNow("second")

	snap, err := h.st.LatestSnapshot(ctx, h.urlID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if snap.RenderMode != model.RenderServerRendered {
		t.Fatalf("normal page render_mode = %q, want %q", snap.RenderMode, model.RenderServerRendered)
	}
	if openHas(t, h.st, h.urlID, "needs_rendering") {
		t.Errorf("a normally server-rendered page must NEVER open a needs_rendering issue")
	}
	for _, e := range h.deps.ingestedEvents() {
		if e.ChangeType == "render_mode" {
			t.Errorf("a normally server-rendered page must never ingest a render_mode alert event; got %+v", e)
		}
	}
	if got := atomic.LoadInt32(h.slackHits); got != 0 {
		t.Errorf("a normally server-rendered page must never alert; slackHits=%d", got)
	}
}

// countRenderModeEvents counts ingested render_mode alert events, asserting each carries the
// warning severity and the expected render_mode + monitored markers in its detail (the
// bridged needs_rendering Finding.Detail rides through to Event.After).
func countRenderModeEvents(t *testing.T, evs []alerts.Event, wantRenderMode, wantMonitored string) int {
	t.Helper()
	n := 0
	for _, e := range evs {
		if e.ChangeType != "render_mode" {
			continue
		}
		n++
		if e.Severity != model.SeverityWarning {
			t.Errorf("the bridged needs_rendering event severity = %q, want warning", e.Severity)
		}
		if !strings.Contains(e.After, wantRenderMode) {
			t.Errorf("render_mode alert detail must name render_mode %q; After=%q", wantRenderMode, e.After)
		}
		if !strings.Contains(e.After, wantMonitored) {
			t.Errorf("render_mode alert detail must carry monitored=%q; After=%q", wantMonitored, e.After)
		}
	}
	return n
}

// assertOnlyExpectedFields fails if any ingested event's ChangeType is outside the allowed
// set — so a fixture that leaked an unintended delta (a stray title/meta change) is caught
// rather than silently widening the proof.
func assertOnlyExpectedFields(t *testing.T, evs []alerts.Event, allowed map[string]bool) {
	t.Helper()
	for _, e := range evs {
		if !allowed[e.ChangeType] {
			t.Errorf("unexpected ingested event field %q (allowed=%v): %+v", e.ChangeType, allowed, e)
		}
	}
}

// openHas reports whether an OPEN issue with the given rule_id exists for urlID.
func openHas(t *testing.T, st *store.DB, urlID int64, ruleID string) bool {
	t.Helper()
	return len(openIssues(t, st, urlID, ruleID)) > 0
}

// openIssues returns the OPEN issues for urlID matching ruleID.
func openIssues(t *testing.T, st *store.DB, urlID int64, ruleID string) []model.Issue {
	t.Helper()
	uid := urlID
	all, err := st.ListIssues(context.Background(), store.IssueFilter{URLID: &uid, OpenOnly: true})
	if err != nil {
		t.Fatalf("ListIssues(open): %v", err)
	}
	var out []model.Issue
	for _, iss := range all {
		if iss.RuleID == ruleID {
			out = append(out, iss)
		}
	}
	return out
}

// closedHas reports whether a CLOSED issue with the given rule_id exists for urlID — the
// observable evidence that the engine lifecycle resolved the incident on recovery.
func closedHas(t *testing.T, st *store.DB, urlID int64, ruleID string) bool {
	t.Helper()
	uid := urlID
	all, err := st.ListIssues(context.Background(), store.IssueFilter{URLID: &uid})
	if err != nil {
		t.Fatalf("ListIssues(all): %v", err)
	}
	for _, iss := range all {
		if iss.RuleID == ruleID && iss.Status == model.IssueClosed {
			return true
		}
	}
	return false
}
