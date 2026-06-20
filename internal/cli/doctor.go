package cli

import (
	"context"
	"io"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/coverage"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
)

// runDoctor runs the honest precheck for url with sitemap auto-counting (pagesOverride
// 0). It is the testable core of the doctor command: tests inject AllowPrivate=true
// pointing at an httptest server, while the cobra RunE wires real config.
func runDoctor(ctx context.Context, w io.Writer, url string, opts precheck.Options, cfg *config.Config) error {
	return runDoctorWithPages(ctx, w, url, opts, cfg, 0)
}

// runDoctorWithPages is runDoctor with an explicit page-count override for the coverage
// estimator. pagesOverride > 0 skips the sitemap auto-count (used by --pages and by the
// estimator tests); pagesOverride == 0 auto-counts the robots-declared sitemap. The
// estimate is printed after the rendering read and before control readiness.
func runDoctorWithPages(ctx context.Context, w io.Writer, url string, opts precheck.Options, cfg *config.Config, pagesOverride int) error {
	rep, err := precheck.Run(ctx, url, opts)
	if err != nil {
		return err
	}

	ew := &errWriter{w: w}

	// ── Verdict line ─────────────────────────────────────────────────────────
	ew.printf("Doctor report for %s\n", url)
	ew.printf("Verdict: %s\n", verdictLabel(rep.Verdict))
	ew.println(rep.Summary)

	// ── Preflight (reused fetcher.Doctor) ────────────────────────────────────
	d := rep.Doctor
	ew.println("\nPreflight:")
	ew.printf("  homepage status: %d\n", d.HomepageStatus)
	ew.printf("  fetch class:     %s\n", d.FetchClass)
	ew.printf("  robots:          %s (status %d)\n", robotsLabel(d.RobotsVerdict), d.RobotsStatus)
	if d.Blocked {
		detector := d.Detector
		if detector == "" {
			detector = "unknown"
		}
		ew.printf("  blocked:         yes (detector: %s)\n", detector)
	}
	if len(d.RedirectChain) > 1 {
		ew.printf("  redirects:       %d hop(s)\n", len(d.RedirectChain)-1)
	}
	if len(d.Egress.IPs) > 0 {
		ew.printf("  egress IP:       %v\n", d.Egress.IPs)
	}

	// ── JS / rendering read ──────────────────────────────────────────────────
	js := rep.JS
	ew.println("\nRendering check:")
	ew.printf("  render mode:     %s (confidence: %s)\n", js.Kind, js.Confidence)
	if js.Framework != "" {
		ew.printf("  framework hint:  %s\n", js.Framework)
	}
	ew.printf("  visible words:   %d\n", js.VisibleWordCount)
	ew.printf("  script bytes:    %d\n", js.ScriptBytes)

	// Every evaluated signal (fired or not) — the verdict is never a black box.
	ew.println("\nSignals:")
	for _, s := range js.Signals {
		ew.printf("  [%s] %-22s %s\n", mark(s.Present), s.Name, s.Detail)
	}

	// ── Honest summary + advice ──────────────────────────────────────────────
	ew.println("")
	ew.println(js.Summary)
	ew.println(js.Advice)

	// ── Coverage estimate (Phase 4) ──────────────────────────────────────────
	// pagesOverride wins; otherwise auto-count the robots-declared sitemap. The
	// auto-count is skipped when cfg is nil (the precheck-only unit tests), keeping
	// them network-deterministic. It reuses ResolveCrawl (the single source of truth)
	// at the verified tier so the estimate shows the site's best achievable speed.
	pages := pagesOverride
	rate, _, recheck := resolveCrawlBudget(cfg, url)
	if pages <= 0 && cfg != nil {
		f := fetcher.New(fetcher.Options{UserAgent: opts.UserAgent, AllowPrivate: opts.AllowPrivate})
		if n, ok := countSitemapPages(ctx, f, opts.UserAgent, url, opts.AllowPrivate); ok {
			pages = n
		}
	}
	ew.println("")
	// doctor reports the raw estimate, not a per-site cap, so pass 0 (uncapped) + perSiteCap=false.
	ew.println(renderCoverageLine(pages, coverage.Estimate(pages, rate), recheck, 0, false))

	// ── Alert channels (decision #23) ────────────────────────────────────────
	// Report the zero-channel state honestly: a monitor with no notifier records
	// changes but tells no one. This is a WARNING, never a failure — alerts are
	// optional (pull surfaces still work). Skipped when cfg is nil (precheck-only
	// unit tests), matching the coverage/control sections.
	if cfg != nil {
		ew.println("\nAlert channels:")
		ew.println(notifierDoctorLine(cfg))
	}

	if ew.err != nil {
		return ew.err
	}
	// Search Console connectivity (GSC W1): when the target site has a `gsc` block,
	// prove the credential authenticates and the property is reachable; otherwise an
	// honest WARNING that GSC intelligence is off. Best-effort, never fatal. Skipped
	// when cfg is nil (precheck-only unit tests stay network-deterministic).
	if cfg != nil {
		if err := runDoctorGSC(ctx, w, cfg, url); err != nil {
			return err
		}
	}
	// Control-plane readiness (D10): proves Hop-2 (daemon reachable + token
	// authenticates), which the precheck above does not. Skipped only when no
	// config is available (e.g. a direct unit test of the precheck rendering).
	if cfg != nil {
		return runDoctorControl(ctx, w, cfg)
	}
	return nil
}

// verdictLabel renders an uppercase, human-readable verdict label.
func verdictLabel(v precheck.Verdict) string {
	switch v {
	case precheck.VerdictGreen:
		return "GREEN"
	case precheck.VerdictRed:
		return "RED"
	default:
		return "YELLOW"
	}
}

// robotsLabel renders the robots verdict, defaulting an empty value to "unknown".
func robotsLabel(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// mark renders a checkbox for a fired/absent signal.
func mark(present bool) string {
	if present {
		return "x"
	}
	return " "
}

// doctorEgressEndpoint resolves the egress-probe endpoint for the doctor command. The
// probe is OFF by default (it would make a one-shot diagnostic silently call a
// third-party IP-echo service): it returns the configured endpoint only when the user
// opts in with --check-egress AND config has the egress check enabled.
func doctorEgressEndpoint(checkEgress, cfgEnabled bool, endpoint string) string {
	if checkEgress && cfgEnabled {
		return endpoint
	}
	return ""
}

// newDoctorCmd builds the `rabbot doctor <url>` command. It loads config to resolve
// the User-Agent and (opt-in) egress endpoint, then calls precheck.Run and renders an
// honest report. It takes BuildInfo because the resolved User-Agent depends on the version.
func newDoctorCmd(bi BuildInfo) *cobra.Command {
	var checkEgress bool
	var pages int
	cmd := &cobra.Command{
		Use:   "doctor <url>",
		Short: "Honest preflight for a URL: reachability, robots, and a JS-rendering hint",
		Long: "doctor runs a one-shot, read-only preflight for a URL — reusing the crawler's " +
			"robots/block checks — and adds a calibrated hint about whether the page's " +
			"SEO content is visible without JavaScript. The JS hint is never definitive: every " +
			"signal is shown, and you are told how to confirm it in a browser. By default it makes " +
			"no third-party calls; pass --check-egress to also probe the outbound IP.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			// Validate the target at the boundary, consistent with every other input
			// path (setup/reconcile/run/discovery): reject non-http(s) schemes and
			// hostless/disallowed-IP targets up front rather than failing late in the
			// transport. Production keeps the SSRF posture (allowPrivate=false); tests
			// call runDoctor directly with AllowPrivate=true against loopback servers.
			if err := fetcher.ValidateSiteURL(args[0], false); err != nil {
				return err
			}
			cfg, err := loadConfig(c)
			if err != nil {
				return err
			}
			egress := doctorEgressEndpoint(checkEgress, cfg.Crawler.EgressCheckEnabled, cfg.Crawler.EgressCheckEndpoint)
			req := fetcher.Request{URL: args[0]}
			access := cfg.AccessForBaseURL(args[0])
			req.Headers = access.Headers
			req.BasicUser = access.BasicUser
			req.BasicPass = access.BasicPass
			req.ProxyURL = access.ProxyURL

			opts := precheck.Options{
				// Single-target diagnostic: compute the per-site UA once for this host
				// (verified=false — doctor runs pre-verification, so the honest signal is
				// state 2 "<site> contact, unverified" on an email-domain match, else state
				// 3 "unverified — confirm or block"). The crawler.user_agent override still
				// wins inside UserAgentFor.
				UserAgent:      cfg.UserAgentFor(hostFromURL(args[0]), bi.Version, false),
				EgressEndpoint: egress,
				Request:        req,
				// Production: keep the SSRF guard on. Tests call runDoctor directly with
				// AllowPrivate=true against a loopback httptest server.
				AllowPrivate: false,
			}
			return runDoctorWithPages(c.Context(), c.OutOrStdout(), args[0], opts, cfg, pages)
		},
	}
	cmd.Flags().BoolVar(&checkEgress, "check-egress", false,
		"also probe the outbound egress IP via the configured endpoint (makes a third-party call)")
	cmd.Flags().IntVar(&pages, "pages", 0,
		"page count for the coverage estimate (0 = auto-count the site's sitemap)")
	return cmd
}
