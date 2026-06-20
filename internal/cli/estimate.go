package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/coverage"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/humanize"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/scheduler"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// maxSitemapDocsForEstimate bounds how many sitemap documents the one-shot estimator
// fetches (a sitemap index can point at many children). A diagnostic must not turn into
// a crawl: cap the fan-out and report the partial count it found.
const maxSitemapDocsForEstimate = 5

// sitemapCountBudget is the OVERALL wall-clock the one-shot sitemap auto-count may take.
// Each individual sitemap fetch carries the fetcher's own 30s timeout, and the count can
// follow several documents (an index plus children, up to maxSitemapDocsForEstimate), so
// a naive serial count could wait out ~150–180s on a hanging host. This bounded child
// context self-caps the whole count so a slow sitemap degrades to "page count unknown"
// instead of stalling `doctor`/`init` for minutes.
const sitemapCountBudget = 12 * time.Second

// renderCoverageLine formats a coverage Result into the one honest line the doctor
// command and the init summary both print. A non-positive page count means the count is
// unknown (no usable sitemap, no --pages) — it prints the spec's guidance rather than a
// fabricated number. recheck is the resolved recheck cadence (the daemon's per-site base
// interval), surfaced as a short suffix; it is distinct from the full-pass time, which is
// the wall-clock to crawl every page exactly once. effCap is the resolved per-site page
// cap (0 = unlimited); when 0 < effCap < pages the site is only partially monitored, so
// the line states "monitoring <cap> of ~<pages> pages (capped — …)" (Spec B D8) so a
// headless operator sees the gap. effCap == 0 or effCap >= pages keeps the full
// "~<pages> pages" phrasing. perSiteCap selects the right remedy: a per-site cap
// (sites[i].discovery.max_pages_per_site) overrides the global default, so changing the
// global has no effect — the operator must edit that key in config.yaml; only a cap that
// comes from the global default is fixable with `config set`.
func renderCoverageLine(pages int, est coverage.Result, recheck time.Duration, effCap int, perSiteCap bool) string {
	if pages <= 0 {
		return "Coverage: page count unknown — pass --pages N to estimate a full crawl pass."
	}
	mb := float64(est.ApproxBytes) / (1024 * 1024)
	var head string
	if effCap > 0 && effCap < pages {
		remedy := "raise/remove with 'rabbot config set defaults.discovery.max_pages_per_site <N|0>'"
		if perSiteCap {
			remedy = "raise/remove this site's discovery.max_pages_per_site in config.yaml"
		}
		head = fmt.Sprintf("Coverage: monitoring %d of ~%d pages (capped — %s, 0 = all)",
			effCap, pages, remedy)
	} else {
		head = fmt.Sprintf("Coverage: ~%d pages", pages)
	}
	line := fmt.Sprintf(
		"%s · ~%.2f req/s · full pass ≈ %s · ~%.1f MB on disk "+
			"(go faster by verifying ownership + raising speed).",
		head, est.ReqPerSec, humanDuration(est.FullPass), mb)
	if recheck > 0 {
		line += fmt.Sprintf(" Rechecks every ~%s.", humanDuration(recheck))
	}
	return line
}

// humanDuration renders a Duration as a compact "Xh Ym" / "Ym Zs" / "Zs" string for the
// coverage line. It delegates to the shared humanize.Duration so the CLI coverage line and
// the wizard cap line agree unit-for-unit (the single implementation lives in
// internal/humanize, a leaf package the wizard can also import).
func humanDuration(d time.Duration) string {
	return humanize.Duration(d)
}

// resolveCrawlBudget returns the per-host rate, concurrency, and recheck interval the
// daemon would crawl `target` at, given the config. It reuses ResolveCrawl (Phase 1, the
// single source of truth) for the SiteConfig matching target's base URL (or the zero
// SiteConfig — and thus the defaults — when the URL is not yet configured). State is
// taken as Verified so the estimate reflects the site's *best* achievable speed; the
// caller's nudge points at verification for sites that have not earned it yet.
//
// concurrency is returned for completeness (it is the real resolved per-host parallelism)
// but does NOT enter the coverage req/s: per-host admission is 1/per_host_rate regardless
// of concurrency (the frontier's single rate.Limiter at burst 1). The recheck interval is
// surfaced as the coverage line's "rechecks every ~X" suffix.
func resolveCrawlBudget(cfg *config.Config, target string) (perHostRate time.Duration, concurrency int, recheck time.Duration) {
	if cfg == nil {
		eff := (&config.Config{}).ResolveCrawl(config.SiteConfig{}, verify.StateVerified)
		return eff.PerHostRate, eff.PerHostConcurrency, eff.MinInterval
	}
	var sc config.SiteConfig
	for _, s := range cfg.Sites {
		if s.URL == target {
			sc = s
			break
		}
	}
	eff := cfg.ResolveCrawl(sc, verify.StateVerified)
	return eff.PerHostRate, eff.PerHostConcurrency, eff.MinInterval
}

// countSitemapPages fetches the robots.txt-declared sitemap(s) for target, parses them
// with the shared scheduler.ParseSitemap, follows at most one level of <sitemapindex>,
// and returns the total URL count. ok is false when no usable sitemap was found (no
// declared sitemap, all fetches failed, or zero entries) — the caller then prints the
// "page count unknown" guidance. allowPrivate mirrors the SSRF posture of the caller
// (tests pass true against loopback; production passes false).
//
// This is a deliberately bounded one-shot count (maxSitemapDocsForEstimate documents),
// not a crawl: it never admits, persists, or recurses past one index level.
//
// The robots cache needs an *http.Client (not the fetcher.Fetcher), so it is built from
// fetcher.GuardedNoRedirectClient, which honors allowPrivate exactly like the page
// fetcher: a no-redirect, SSRF-guarded client in production; a loopback-reaching client
// in the test suite. The fetcher.Fetcher f is reused for the actual sitemap fetches.
func countSitemapPages(ctx context.Context, f fetcher.Fetcher, userAgent, target string, allowPrivate bool) (int, bool) {
	// Bound the WHOLE count: each fetch carries the fetcher's 30s timeout and the count
	// can follow several documents, so without an overall deadline a hanging sitemap host
	// could stall the diagnostic for minutes. This child context self-caps the count so a
	// slow site degrades to "page count unknown" rather than blocking the caller.
	ctx, cancel := context.WithTimeout(ctx, sitemapCountBudget)
	defer cancel()

	rc := frontier.NewRobotsCache(fetcher.GuardedNoRedirectClient(30*time.Second, allowPrivate), userAgent, 5*time.Minute)
	declared := rc.Sitemaps(ctx, target)
	if len(declared) == 0 {
		return 0, false
	}

	total := 0
	docs := 0
	queue := append([]string(nil), declared...)
	for len(queue) > 0 && docs < maxSitemapDocsForEstimate {
		u := queue[0]
		queue = queue[1:]
		if fetcher.ValidateSiteURL(u, allowPrivate) != nil {
			continue
		}
		docs++
		res, err := f.Fetch(ctx, fetcher.Request{URL: u})
		if err != nil || res.FetchClass != model.FetchOK || len(res.Body) == 0 || res.Truncated {
			continue
		}
		entries, isIndex, perr := scheduler.ParseSitemap(res.Body)
		if perr != nil {
			continue
		}
		if isIndex {
			for _, e := range entries {
				queue = append(queue, e.Loc)
			}
			continue
		}
		total += len(entries)
	}
	if total == 0 {
		return 0, false
	}
	return total, true
}

// emitCoverageForAddedSites prints a one-line coverage estimate for each freshly added
// site, after the init "added site:" lines. It is best-effort: the sitemap auto-count
// makes one bounded outbound request per site and never fails setup — an uncountable
// site prints the "page count unknown — pass --pages N" guidance instead. allowPrivate
// mirrors the caller's SSRF posture (production passes false). version is the BuildInfo
// version threaded from the caller so the self-identifying User-Agent carries the real
// "Rabbot-SEO/<version>" rather than an empty version segment (finding #9).
func emitCoverageForAddedSites(ctx context.Context, out io.Writer, cfg *config.Config, addedURLs []string, version string, allowPrivate bool) {
	if cfg == nil {
		return
	}
	for _, u := range addedURLs {
		// Single-target: compute the per-site UA for this host once (verified=false —
		// a freshly added site is pre-verification). The fetcher is built per URL so
		// each carries the right self-identifying UA; the count is one bounded request.
		ua := cfg.UserAgentFor(hostFromURL(u), version, false)
		f := fetcher.New(fetcher.Options{UserAgent: ua, AllowPrivate: allowPrivate})
		rate, _, recheck := resolveCrawlBudget(cfg, u)
		pages := 0
		if n, ok := countSitemapPages(ctx, f, ua, u, allowPrivate); ok {
			pages = n
		}
		// Resolve the per-site cap (0 = unlimited) so the line can flag a partial
		// coverage (Spec B D8). The zero SiteConfig falls back to the defaults; a
		// configured site matches by URL.
		var sc config.SiteConfig
		for _, s := range cfg.Sites {
			if s.URL == u {
				sc = s
				break
			}
		}
		effCap := cfg.ResolveDiscovery(sc).MaxPages
		perSiteCap := sc.Discovery.MaxPagesPerSite != nil
		_, _ = fmt.Fprintf(out, "%s\n  %s\n", u, renderCoverageLine(pages, coverage.Estimate(pages, rate), recheck, effCap, perSiteCap))
	}
}

// productionCountPages builds the wizard's CountPages seam: a bounded, SSRF-guarded
// (allowPrivate=false) sitemap auto-count over a single shared fetcher.New, reusing
// the exact posture emitCoverageForAddedSites uses. The wizard calls this on URL
// entry in a background goroutine that it ctx-cancels when the form ends (Phase 4
// startCount); the ~12s budget lives inside countSitemapPages. Built per launch so
// the closure captures the operator's resolved self-identifying User-Agent. There
// is exactly ONE production binding — no bgPageCount/sitemapCounter variants.
//
// version is the BuildInfo version threaded from the caller so the self-identifying
// User-Agent carries the real "Rabbot-SEO/<version>" rather than an empty version
// segment (finding #9). It delegates to productionCountPagesFor with the production
// SSRF posture (allowPrivate=false), the only knob a test relaxes.
func productionCountPages(cfg *config.Config, version string) func(ctx context.Context, url string) (int, bool) {
	return productionCountPagesFor(cfg, version, false)
}

// productionCountPagesFor is the testable core of productionCountPages: it takes the
// SSRF posture explicitly so tests can point it at a loopback httptest server with
// allowPrivate=true, while production always passes false. The operator can type any
// URL here, so the UA is resolved PER CALL for the typed host (verified=false — this
// runs during onboarding, pre-verification): the shared fetcher carries a per-host UA
// func, and the robots count gets the same per-host UA string. The override inside
// UserAgentFor still wins.
func productionCountPagesFor(cfg *config.Config, version string, allowPrivate bool) func(ctx context.Context, url string) (int, bool) {
	uaFor := func(host string) string { return cfg.UserAgentFor(host, version, false) }
	f := fetcher.New(fetcher.Options{UserAgent: cfg.ResolvedUserAgent(version), UserAgentFunc: uaFor, AllowPrivate: allowPrivate})
	return func(ctx context.Context, url string) (int, bool) {
		return countSitemapPages(ctx, f, uaFor(hostFromURL(url)), url, allowPrivate)
	}
}
