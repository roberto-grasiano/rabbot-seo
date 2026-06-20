package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/benchcorpus"
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

// ---------------------------------------------------------------------------
// B3 recheck-throughput benchmark — PIPELINE CPU cost of one CrawlOne over the
// full M1+M2 pipeline (frontier gate -> fetch -> extract -> diff -> rules ->
// alert ingest), against a deterministic benchcorpus page served from loopback.
//
// What this measures, and what it deliberately does NOT:
//   - It measures the per-page PIPELINE cost the daemon pays once a URL is
//     admitted: HTTP round-trip to loopback, the TWO HTML parses extract does
//     (goquery in ExtractWith + a second full parse inside MainText — the known
//     F53 double-parse, QUANTIFIED here, not fixed), the diff, the rule pass,
//     and the alert-pipeline ingest/dedup write.
//   - It does NOT measure the deliberate 1/per_host_rate politeness wait. The
//     frontier is constructed with PerHostRate = time.Nanosecond, the SMALLEST
//     non-zero spacing: a zero/negative rate is rewritten by frontier.New to a
//     10s default (frontier.go:66), so passing 0 would measure admission wait,
//     not pipeline cost. The published doc presents the polite capacity number
//     (1/per_host_rate) SEPARATELY and must never imply the binary crawls real
//     sites at pipeline speed.
//
// The three scenarios trace the three recheck outcomes a real monitor sees,
// from cheapest to most expensive:
//   - not_modified: the server 304s the conditional GET CrawlOne sends -> NO
//     body, extraction and SaveSnapshot are SKIPPED (the cheap common case).
//   - unchanged:    an identical 200 each time -> extract + SaveSnapshot run,
//     but diff.Compare finds zero changes (no change rows, no alerts).
//   - changed:      a body whose content + title mutate every fetch -> the full
//     diff -> rules -> alert-ingest path runs and writes >= 1 change row.
//
// TestRecheckBenchHarness is the falsifiable anti-no-op guard: it runs ONE
// iteration of each scenario and asserts the claimed path was exercised by
// reading REAL store state (change rows / snapshot presence), not bench
// counters. A bench that silently always-304'd (publishing a fake-fast number)
// would fail that test.
// ---------------------------------------------------------------------------

// recheckMode selects how the corpus server answers a page request.
type recheckMode int

const (
	// modeNotModified: the server answers 304 to the conditional GET (the URL
	// carries an ETag, so CrawlOne sends If-None-Match) — extraction + persistence
	// are skipped by the crawl pipeline.
	modeNotModified recheckMode = iota
	// modeUnchanged: the server returns the identical 200 body every time, with no
	// validators, so no conditional GET short-circuits and every fetch extracts +
	// persists, but diff.Compare sees no change.
	modeUnchanged
	// modeChanged: the server returns a body whose <title> and main content mutate
	// on every request (a monotonic request counter is woven into both), so each
	// recheck diffs substantively against the prior snapshot.
	modeChanged
)

// benchETag is the validator the modeNotModified URL carries and the corpus
// server echoes a 304 against. Any other mode neither sends nor honors it.
const benchETag = `"rabbot-bench-v1"`

// recheckHarness bundles the full pipeline plus the live store so both the
// benchmark and the anti-no-op test drive identical wiring.
type recheckHarness struct {
	crawler *Crawler
	store   *store.DB
	srv     *httptest.Server
	mode    *atomic.Int32 // recheckMode; settable per scenario without rebuilding
	reqs    *atomic.Int64 // monotonic page-request counter (drives modeChanged mutation)
}

// newRecheckHarness stands up the entire recheck pipeline against a loopback
// corpus server and a temp-FILE store. The frontier spacing is time.Nanosecond
// (NOT zero — frontier.New rewrites <= 0 to 10s) so the admission wait is
// effectively removed and we measure pipeline CPU cost. Alerts route to a
// loopback discard webhook (200, body dropped) so the alert path is exercised
// with zero egress. now is the fixed clock the whole pipeline shares.
func newRecheckHarness(tb testing.TB, now func() time.Time) *recheckHarness {
	tb.Helper()
	ctx := context.Background()

	mode := &atomic.Int32{}
	reqs := &atomic.Int64{}

	// The corpus origin: a 404 on /robots.txt (RobotsCache.Allowed returns true on
	// a fetch error, so everything is admitted with no robots seeding). The page
	// path answers per mode. A loopback httptest server binds 127.0.0.1 only.
	page := benchcorpus.Page(benchcorpus.Article, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch recheckMode(mode.Load()) {
		case modeNotModified:
			// Conditional GET: a matching validator -> 304 (no body). The bench URL
			// always carries this ETag, so the very first fetch already 304s.
			if r.Header.Get("If-None-Match") == benchETag {
				w.Header().Set("ETag", benchETag)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			// No/mismatched validator (should not happen for the bench URL): serve the
			// page with the ETag so a subsequent conditional GET can 304.
			w.Header().Set("ETag", benchETag)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(page)
		case modeChanged:
			// Mutate BOTH the <title> and the main content every request so each
			// recheck diffs substantively against the prior snapshot. The counter is
			// woven into a fresh <title> and an extra content paragraph; the rest of
			// the corpus page (head metadata, links, images) is unchanged, isolating
			// the title + content change.
			n := reqs.Add(1)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(mutatedPage(page, n))
		default: // modeUnchanged
			// Identical 200 body every time, NO validators (so no conditional GET):
			// the pipeline extracts + persists, and diff.Compare finds no change.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(page)
		}
	}))
	tb.Cleanup(srv.Close)

	st, err := store.Open(ctx, filepath.Join(tb.TempDir(), "recheck-bench.db"))
	if err != nil {
		tb.Fatalf("store.Open: %v", err)
	}
	tb.Cleanup(func() { _ = st.Close() })

	// Seed the parent rows: PRAGMA foreign_keys=ON means snapshots/changes must
	// reference an existing site + url.
	siteID, err := st.AddSite(ctx, model.Site{
		BaseURL: srv.URL, Name: "Bench", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		tb.Fatalf("AddSite: %v", err)
	}
	if _, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: srv.URL + "/article/1", FirstSeen: now(), NextCheckAt: now(), Interval: 600, Importance: 1.0,
	}); err != nil {
		tb.Fatalf("UpsertURL: %v", err)
	}

	// Alerts -> a loopback DISCARD webhook (200, body dropped): the alert pipeline
	// is fully exercised (ingest/dedup/incident writes hit the real store) with
	// zero egress to any third party.
	discard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	tb.Cleanup(discard.Close)

	notifier := notify.NewSlackNotifier("slack-all", discard.URL, discard.Client())
	registry := notify.NewRegistry(
		map[string]notify.Notifier{"slack-all": notifier},
		[]config.RouteConfig{{Match: map[string]string{}, Notifier: "slack-all"}},
	)
	dispatcher := notify.NewDispatcher(registry)
	// HourlyCap is set very high so the per-iteration changed scenario never trips
	// the rate cap mid-benchmark (which would change what the alert path does from
	// one iteration to the next). DedupWindow stays realistic.
	pipeline := alerts.NewPipeline(st, dispatcher,
		alerts.WithCaps(alerts.Caps{DedupWindow: 5 * time.Minute, HourlyCap: 1 << 30, IncidentAutoClose: 24 * time.Hour}),
		alerts.WithClock(now),
	)
	engine := rules.NewEngine(rules.DefaultRuleSet(), st, now)
	proc := NewProcessor(&e2eDeps{store: st, engine: engine, pipeline: pipeline}, diff.DefaultSimhashThreshold, now)

	crawler := &Crawler{
		Store:     st,
		Fetcher:   fetcher.New(fetcher.Options{UserAgent: "Rabbot-SEO/bench", Timeout: 30 * time.Second, MaxBodyBytes: 5 << 20, AllowPrivate: true}),
		Extractor: extract.NewExtractor(),
		Robots:    frontier.NewRobotsCache(srv.Client(), "Rabbot-SEO/bench", time.Minute),
		// time.Nanosecond — the SMALLEST non-zero spacing. Zero/negative would be
		// rewritten to a 10s default (frontier.go:66), turning the bench into an
		// admission-wait measurement instead of pipeline cost.
		Frontier:  frontier.New(frontier.Options{PerHostRate: time.Nanosecond, PerHostConcurrency: 1 << 20}),
		Now:       now,
		Processor: proc,
	}

	return &recheckHarness{crawler: crawler, store: st, srv: srv, mode: mode, reqs: reqs}
}

// urlFor returns the bench URL row for the given mode. modeNotModified carries
// the ETag so CrawlOne sends If-None-Match and the server 304s from request #1.
func (h *recheckHarness) urlFor(mode recheckMode) model.URL {
	u := model.URL{ID: 1, SiteID: 1, URL: h.srv.URL + "/article/1", Interval: 600, Importance: 1.0}
	if mode == modeNotModified {
		u.ETag = benchETag
	}
	return u
}

// crawl runs one CrawlOne for the given mode. minInterval/maxInterval mirror the
// daemon's defaults; the empty content selector is the shipped M1 default.
func (h *recheckHarness) crawl(ctx context.Context, mode recheckMode) CrawlResult {
	h.mode.Store(int32(mode))
	return h.crawler.CrawlOne(ctx, h.urlFor(mode), 600, 86400, "")
}

// mutatedPage returns the corpus page with its <title> replaced and a unique
// content paragraph appended, both keyed on n, so consecutive fetches differ in
// BOTH the title field and the main-content hash. It rebuilds the bytes by hand
// (no extra dependency) and is deterministic for a given n.
func mutatedPage(base []byte, n int64) []byte {
	html := string(base)
	// Replace the existing <title>…</title> with a counter-bearing one so the
	// extracted Title differs each fetch (a title change is always substantive).
	const titleOpen, titleClose = "<title>", "</title>"
	if i := strings.Index(html, titleOpen); i >= 0 {
		if j := strings.Index(html[i:], titleClose); j >= 0 {
			j += i
			html = html[:i+len(titleOpen)] + fmt.Sprintf("Bench rev %d", n) + html[j:]
		}
	}
	// Append a unique paragraph just before </main> so the readability main text
	// (and thus the content hash + simhash) shifts substantively each fetch.
	const endMain = "</main>"
	extra := fmt.Sprintf("<p>Revision %d distinct body sentence alpha bravo charlie delta echo foxtrot golf hotel.</p>\n", n)
	if i := strings.Index(html, endMain); i >= 0 {
		html = html[:i] + extra + html[i:]
	} else {
		html += extra
	}
	return []byte(html)
}

// BenchmarkRecheckPipeline measures the PIPELINE CPU cost of one CrawlOne for
// each recheck outcome. The admission wait is zeroed (frontier spacing =
// time.Nanosecond) so the number reflects fetch + parse + diff + rules + ingest,
// NOT politeness. b.ReportAllocs is on per the B3 allocation-honesty rule.
func BenchmarkRecheckPipeline(b *testing.B) {
	now := func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }
	ctx := context.Background()

	scenarios := []struct {
		name string
		mode recheckMode
	}{
		{"not_modified", modeNotModified},
		{"unchanged", modeUnchanged},
		{"changed", modeChanged},
	}

	for _, sc := range scenarios {
		b.Run(sc.name, func(b *testing.B) {
			h := newRecheckHarness(b, now)
			h.mode.Store(int32(sc.mode))
			u := h.urlFor(sc.mode)

			// Prime once so steady-state cost (a recheck against an existing
			// snapshot) is what we measure, not the first-fetch baseline.
			_ = h.crawler.CrawlOne(ctx, u, 600, 86400, "")

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res := h.crawler.CrawlOne(ctx, u, 600, 86400, "")
				if res.Err != nil {
					b.Fatalf("CrawlOne: %v", res.Err)
				}
			}
		})
	}
}

// TestRecheckBenchHarness is the falsifiable anti-no-op guard (B3 criterion 2):
// it proves each benchmark scenario exercises the path it claims by reading REAL
// store state after one iteration, not by trusting bench-internal counters.
//
//   - changed     -> at least one stored change row (diff -> RecordChanges ran).
//   - unchanged   -> zero change rows (extract + save ran, but diff found nothing).
//   - not_modified -> NO snapshot was ever persisted (the 304 skipped SaveSnapshot).
//
// A bench that silently always-304'd would fail the `changed`/`unchanged` arms;
// one that always extracted would fail the `not_modified` arm.
func TestRecheckBenchHarness(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }
	ctx := context.Background()

	t.Run("changed_records_change_rows", func(t *testing.T) {
		h := newRecheckHarness(t, now)
		// First crawl establishes the baseline snapshot (no prior -> no change rows).
		if res := h.crawl(ctx, modeChanged); res.Err != nil {
			t.Fatalf("baseline crawl: %v", res.Err)
		}
		base, err := h.store.GetURLHistory(ctx, 1, time.Time{})
		if err != nil {
			t.Fatalf("GetURLHistory (baseline): %v", err)
		}
		if len(base) != 0 {
			t.Fatalf("first crawl has no prior snapshot, so it must record ZERO changes; got %d", len(base))
		}
		// Second crawl: the server mutates the body, so this diffs substantively
		// against the just-saved baseline and must record at least one change row.
		if res := h.crawl(ctx, modeChanged); res.Err != nil {
			t.Fatalf("changed crawl: %v", res.Err)
		}
		changes, err := h.store.GetURLHistory(ctx, 1, time.Time{})
		if err != nil {
			t.Fatalf("GetURLHistory (changed): %v", err)
		}
		if len(changes) == 0 {
			t.Fatalf("the changed scenario must record >= 1 change row (diff -> RecordChanges); got 0")
		}
		// And the change must be substantive (the title + content mutated), proving
		// we exercised the diff -> rules -> alert path, not cosmetic churn.
		var sawSubstantive bool
		for _, c := range changes {
			if c.ChangeClass == model.ChangeSubstantive {
				sawSubstantive = true
			}
		}
		if !sawSubstantive {
			t.Errorf("the changed scenario must produce a substantive change; got only cosmetic: %+v", changes)
		}
	})

	t.Run("unchanged_records_no_change_rows", func(t *testing.T) {
		h := newRecheckHarness(t, now)
		// Baseline, then an identical refetch. Both extract + save; the second diffs
		// against the first and must find nothing.
		if res := h.crawl(ctx, modeUnchanged); res.Err != nil {
			t.Fatalf("baseline crawl: %v", res.Err)
		}
		if res := h.crawl(ctx, modeUnchanged); res.Err != nil {
			t.Fatalf("unchanged crawl: %v", res.Err)
		}
		changes, err := h.store.GetURLHistory(ctx, 1, time.Time{})
		if err != nil {
			t.Fatalf("GetURLHistory: %v", err)
		}
		if len(changes) != 0 {
			t.Fatalf("an identical refetch must record ZERO change rows; got %d (%+v)", len(changes), changes)
		}
		// Sanity: a snapshot WAS persisted (extract + save ran), distinguishing this
		// from the not_modified path. LatestSnapshot must succeed.
		if _, err := h.store.LatestSnapshot(ctx, 1); err != nil {
			t.Fatalf("the unchanged scenario must persist a snapshot (extract + save ran); LatestSnapshot: %v", err)
		}
	})

	t.Run("not_modified_skips_snapshot", func(t *testing.T) {
		h := newRecheckHarness(t, now)
		// The bench URL carries the ETag, so the server 304s from request #1 and the
		// crawl pipeline skips extraction + SaveSnapshot entirely.
		res := h.crawl(ctx, modeNotModified)
		if res.Err != nil {
			t.Fatalf("not_modified crawl: %v", res.Err)
		}
		if res.FetchClass != model.FetchOK {
			t.Fatalf("a 304 classifies as FetchOK (NotModified); got %q", res.FetchClass)
		}
		// No snapshot was persisted: LatestSnapshot must report ErrNotFound. This is
		// the falsifiable proof the extract+persist path was SKIPPED.
		if _, err := h.store.LatestSnapshot(ctx, 1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the not_modified scenario must persist NO snapshot (304 skips SaveSnapshot); LatestSnapshot err = %v, want ErrNotFound", err)
		}
		// And no change rows either (nothing to diff).
		changes, err := h.store.GetURLHistory(ctx, 1, time.Time{})
		if err != nil {
			t.Fatalf("GetURLHistory: %v", err)
		}
		if len(changes) != 0 {
			t.Fatalf("a 304 records no changes; got %d (%+v)", len(changes), changes)
		}
	})
}
