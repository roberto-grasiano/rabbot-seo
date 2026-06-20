package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/discovery"
	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/linkgraph"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/notify"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/scheduler"
	"github.com/roberto-grasiano/rabbot-seo/internal/segments"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/supervisor"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// daemonOptions are the resolved inputs to runDaemon.
type daemonOptions struct {
	ConfigPath          string
	DataDir             string
	ControlToken        string
	ControlPort         int // 0 => do not start the control server (tests)
	Version             string
	LogLevel            string
	TickInterval        time.Duration
	EgressCheckEndpoint string // outbound-IP echo endpoint for the richer Status hook
	EgressCheckEnabled  bool   // when false, the outbound egress-IP probe is skipped
	// MetricsAddr is the read-only Prometheus /metrics listen address (host:port).
	// Empty => no metrics listener (off by default, B2). A non-empty value binds
	// the GET-only, unauthenticated metrics server; a bind failure is a fatal
	// startup error (the same F18 pattern as the control server), and a
	// non-loopback bind logs a startup warning.
	MetricsAddr string
	// AllowPrivate disables the crawl fetchers' SSRF guard so the daemon may dial
	// loopback/private hosts. It exists ONLY for end-to-end tests that drive the
	// real daemon against a loopback httptest origin (production never sets it: the
	// run command leaves it false, so internal ranges stay rejected). It mirrors
	// fetcher.Options.AllowPrivate and also relaxes the add_site admission guard so
	// a loopback site can be added in those tests.
	AllowPrivate bool
}

func newRunCmd(bi BuildInfo) *cobra.Command {
	var foreground bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the daemon loop (also what the service runs)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			cfgDir, err := config.ResolveConfigDir()
			if err != nil {
				return err
			}
			cfgPath := config.ConfigFilePath(cfgDir)
			c, err := config.Load(cfgPath, nil)
			if err != nil {
				return err
			}
			dataDir, err := config.ResolveDataDir(c.DataDir)
			if err != nil {
				return err
			}
			token, err := control.LoadOrCreateToken(filepath.Join(cfgDir, "control.token"))
			if err != nil {
				return err
			}
			return runDaemon(ctx, cmd.OutOrStdout(), daemonOptions{
				ConfigPath:          cfgPath,
				DataDir:             dataDir,
				ControlToken:        token,
				ControlPort:         c.Control.Port,
				Version:             bi.Version,
				LogLevel:            c.Log.Level,
				TickInterval:        time.Second,
				EgressCheckEndpoint: c.Crawler.EgressCheckEndpoint,
				EgressCheckEnabled:  c.Crawler.EgressCheckEnabled,
				MetricsAddr:         c.Metrics.Addr,
			})
		},
	}
	// TODO(M3): wire --foreground to a real detach path. Today the service always
	// runs in the foreground (the OS service manager handles daemonization via
	// Arguments: []string{"run"}), so the flag is accepted but inert until M3
	// distribution/packaging lands.
	cmd.Flags().BoolVar(&foreground, "foreground", false, "run in the foreground (do not detach)")
	_ = foreground // reserved; see TODO above
	return cmd
}

// resolveDaemonInstanceKey loads (or, on first run, mints) the per-instance
// secret key for the DAEMON path, applying the spec's fail-safe: a present-but-
// unreadable / malformed key must NOT crash the daemon. On success it returns the
// key; on any error (a malformed/bad-perms key file) it logs the condition and
// returns nil so verify.Verify — which fails safe on a zero-length key, returning
// {throttled, unreachable} with no error — demotes affected sites to throttled
// rather than aborting the whole crawl/alert/control stack.
//
// The error string is logged, NEVER the key bytes. The first-run mint path is the
// ENOENT case, handled inside LoadOrCreateInstanceKey (it writes a fresh 0600
// key), so this only degrades when a key file exists but cannot be trusted. The
// CLI verify path intentionally surfaces this as a hard error; only the daemon
// degrades (spec §Security / §Error handling).
func resolveDaemonInstanceKey(path string, logger *slog.Logger) []byte {
	key, err := verify.LoadOrCreateInstanceKey(path)
	if err != nil {
		logger.Error("instance key unreadable; affected sites resolve to throttled (fail-safe)",
			obs.KeyComponent, "supervisor", obs.KeyError, err.Error())
		return nil
	}
	return key
}

// daemonVerify backs POST /v1/verify inside the daemon: it resolves the site host,
// then runs verify.Begin (no DB write) or verify.Check (the daemon is the single
// writer). The token is DERIVED from instKey — never caller-supplied. A nil/empty
// instKey makes check fail safe ({throttled, unreachable}); begin returns an
// ErrBadRequest on an unknown method/action or missing site (caller faults wrap
// control.ErrBadRequest -> HTTP 400). The response token is the public proof
// token, safe to emit.
func daemonVerify(ctx context.Context, db *store.DB, instKey []byte, req control.VerifyRequest) (control.VerifyResponse, error) {
	method, merr := parseMethod(req.Method)
	if merr != nil {
		return control.VerifyResponse{}, fmt.Errorf("%w: %w", control.ErrBadRequest, merr)
	}
	site, gerr := db.GetSite(ctx, req.SiteID)
	if gerr != nil {
		if errors.Is(gerr, store.ErrNotFound) {
			return control.VerifyResponse{}, fmt.Errorf("%w: site %d not found", control.ErrBadRequest, req.SiteID)
		}
		return control.VerifyResponse{}, gerr
	}
	host := hostFromURL(site.BaseURL)

	switch req.Action {
	case "begin":
		// begin must not write; an empty key here is a caller-visible setup fault, but
		// the daemon loaded/minted the key at startup, so an empty key means the
		// fail-safe degrade path (logged at startup) — report it as a 400 so the tool
		// surfaces a clear message rather than a 500.
		res, berr := verify.Begin(req.SiteID, host, method, instKey)
		if berr != nil {
			return control.VerifyResponse{}, fmt.Errorf("%w: %w", control.ErrBadRequest, berr)
		}
		// State on begin reflects the CURRENT stored tier (unchanged — begin writes
		// nothing). Default to throttled; read the live record best-effort.
		state := string(verify.StateThrottled)
		if rec, rerr := db.GetVerification(ctx, req.SiteID); rerr == nil && rec.State != "" {
			state = string(rec.State)
		}
		return control.VerifyResponse{
			SiteID:       req.SiteID,
			Method:       string(method),
			Token:        res.Token,
			State:        state,
			Instructions: res.Instructions,
			Throttled:    state != string(verify.StateVerified),
		}, nil
	case "check":
		out, cerr := verify.Check(ctx, db, req.SiteID, host, method, verify.Options{
			Now: time.Now().UTC(),
			Key: instKey,
		})
		// A clean miss/unreachable is DATA (Reason set, throttled), not an error: the
		// attempt completed. Only a genuine store-save failure is an error -> 500.
		if cerr != nil && out.Record.State == "" {
			return control.VerifyResponse{}, cerr
		}
		state := string(out.Record.State)
		return control.VerifyResponse{
			SiteID:    req.SiteID,
			Method:    string(method),
			Token:     out.Record.Token,
			State:     state,
			Reason:    string(out.Reason),
			Throttled: state != string(verify.StateVerified),
		}, nil
	default:
		return control.VerifyResponse{}, fmt.Errorf("%w: unknown action %q (want begin|check)", control.ErrBadRequest, req.Action)
	}
}

// runDaemon opens the store, assembles the M1 crawl pipeline (fetcher, robots,
// frontier, extractor, scheduler), starts the (optional) control server wired to
// every M1 hook, runs the scheduler loop, and shuts everything down cleanly on
// ctx cancel.
func runDaemon(ctx context.Context, out io.Writer, opts daemonOptions) error {
	// Derive a cancellable child context so runDaemon can tear down the pipeline
	// goroutines itself on a fatal startup error (e.g. a control-server bind
	// failure, F18) without depending on the parent ctx being cancelled.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Load config FIRST so the logger can honor the advertised log.file knob. An
	// empty ConfigPath (tests) => built-in defaults. A real path that fails to load
	// is fatal unless ctx was cancelled (graceful shutdown). Until the logger is
	// built we have no place to log a graceful-shutdown line, so a cancelled-ctx
	// load failure simply returns nil (the caller observes the clean exit).
	cfg := config.Defaults()
	if opts.ConfigPath != "" {
		c, err := config.Load(opts.ConfigPath, nil)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		cfg = c
	}

	// Build the logger. When log.file is configured, route structured logs to a
	// size-rotating file writer (the advertised log.file knob — previously a dead
	// knob: the logger always wrote to `out` and cfg.Log.File was never consumed).
	// The caller owns the writer's lifecycle, so it is closed on daemon shutdown.
	// An empty log.file keeps the original behavior (write to `out`).
	logWriter := out
	if cfg.Log.File != "" {
		w := obs.FileWriter(cfg.Log.File)
		defer func() { _ = w.Close() }()
		logWriter = w
	}
	logger := obs.NewLogger(logWriter, opts.LogLevel)
	logger.Info("daemon starting", obs.KeyComponent, "supervisor")

	// Non-fatal contact-email guard (finding #5): the crawler announces itself to every
	// origin via a User-Agent carrying crawler.contact_email. An empty/invalid email
	// leaves site owners no way to reach the operator (and the scaffold ships it empty on
	// purpose, finding #7), so warn loudly at startup — but DON'T fail: a running monitor
	// that no one can contact still beats no monitor, and forcing setup here would break
	// existing deployments. The value is the operator's own email (not a secret), so it is
	// safe to surface the validation reason; we still avoid echoing the address itself.
	if verr := config.ValidateEmail(cfg.Crawler.ContactEmail); verr != nil {
		logger.Warn("crawler.contact_email is empty or invalid; site owners cannot reach you — run `rabbot init`",
			obs.KeyComponent, "supervisor", obs.KeyError, verr.Error())
	}

	// Non-fatal zero-channel guard (decision #23): a monitor with no notifier
	// configured records every change but tells no one — it has no real-time value.
	// Warn loudly at startup (once, here — never per tick) and point at the wizard,
	// but DON'T block: pull surfaces (`rabbot report`, MCP) still work, and forcing
	// alerts here would break deliberate pull-only deployments. The message carries
	// no secret.
	if msg, warn := startupNotifierWarning(cfg); warn {
		logger.Warn(msg, obs.KeyComponent, "supervisor")
	}

	db, err := store.Open(ctx, filepath.Join(opts.DataDir, "rabbot.db"))
	if err != nil {
		// A cancelled context during startup (e.g. SIGTERM arriving mid-migration)
		// is a graceful shutdown request, not a failure: store.Open rolls back any
		// in-flight migration transaction, so the DB is left consistent. Mirror the
		// scheduler loop's "ctx cancel => clean exit" semantics rather than crashing.
		if ctx.Err() != nil {
			logger.Info("daemon stopped", obs.KeyComponent, "supervisor")
			return nil
		}
		return err
	}
	defer func() { _ = db.Close() }()

	// Load the per-instance secret key once at startup. The re-verify loop derives
	// each site's expected token from it (it never trusts a stored/caller token), so
	// both the startup and periodic re-verify passes thread it through. It lives
	// alongside the database in the already-resolved opts.DataDir (the same dir the
	// DB was just opened in, above) so the daemon and the CLI agree on its location
	// regardless of a DataDir override. The path does not change at runtime, so it is
	// safe to load once here, outside cfgMu, rather than per pass under the lock.
	// Fail-safe (spec §Security / §Error handling): a present-but-unreadable /
	// malformed key must NOT crash the daemon. resolveDaemonInstanceKey logs the
	// condition and returns nil, and verify.Verify treats a nil/empty key as a
	// clean verified->throttled demotion (no false "verified") — so the crawl,
	// alert, and control surfaces keep running, just throttled, instead of aborting.
	instKey := resolveDaemonInstanceKey(filepath.Join(opts.DataDir, "instance.key"), logger)

	started := time.Now()

	// ── Self-observability metrics (B2) ─────────────────────────────────────
	// Build the per-daemon Prometheus registry + instrument set. It is always
	// constructed (cheap; the GaugeFuncs read atomics/closures), so instrumentation
	// choke points can hold a non-nil *Metrics unconditionally; the read-only
	// listener that EXPOSES it is opened only when metrics.addr is set (below).
	metrics := obs.NewMetrics(opts.Version)

	// cfgMu serializes access to cfg. The control server dispatches each request
	// on its own goroutine and the reload hook (invoked from /v1/reload and every
	// config-mutating hook: sites add/remove, config set) both reassigns and reads
	// cfg, so concurrent control calls would otherwise race on this shared value.
	// It is also taken (per call) by the discoveryResolver below, the
	// sitemap-refresh ticker, and the per-host User-Agent closure, all of which must
	// read the LIVE cfg after a reload. The lock is held only for the brief
	// reassign+snapshot, never across the I/O of reconcileSites, which operates on a
	// private copy. Declared up here so the per-host UA closure (below) can capture
	// it before the fetchers it threads through are constructed.
	var cfgMu sync.Mutex

	// cfgWriteMu serializes the WHOLE config-mutating control flow — the on-disk
	// config.yaml read-modify-write (config.AddSiteYAML / RemoveSiteYAML /
	// SetKeyYAML) PLUS the d.Reload() that re-syncs it into the DB — for the three
	// mutating hooks (add_site / remove_site / set_config). The std http.Server
	// dispatches each request on its own goroutine; cfgMu only guards the in-memory
	// cfg inside Reload(), NOT the file RMW, so concurrent mutating calls otherwise
	// interleave their read-modify-write of config.yaml and silently lose most
	// updates (High#1: 20 concurrent add_site -> ~3 persisted, config.yaml diverges
	// from the DB). This is a SEPARATE lock from cfgMu — it is held across file I/O
	// + reconcile (long), whereas cfgMu is held only for the brief in-memory
	// reassign/snapshot inside Reload and the resolver closures; nesting is one-way
	// (a mutating hook takes cfgWriteMu, then Reload briefly takes cfgMu inside it),
	// so the two never deadlock. It also covers add_site's duplicate pre-check ->
	// write check-then-act window so a concurrent add of the same URL cannot slip
	// between the GetSiteByBaseURL lookup and the AddSiteYAML write.
	var cfgWriteMu sync.Mutex

	// ── Crawl pipeline ──────────────────────────────────────────────────────
	// User-Agent identifies the crawler to origins (config override or the
	// default Rabbot-SEO/<version> (+contact) form). The daemon crawls many
	// hosts through one shared fetcher, so it threads a PER-HOST UA carrying the
	// per-site trust signal (verified-for / contact-unverified / confirm-or-block)
	// resolved at fetch time; uaFunc is wired into the crawl fetcher, the sitemap
	// fetcher, and the robots cache so robots/sitemap/page fetches all present the
	// same per-host identity. The static `ua` remains the host-agnostic fallback
	// (and what host-less contexts use).
	ua := cfg.ResolvedUserAgent(opts.Version)
	// verifiedHosts is the hot-path host->verified cache the per-host UA closure reads
	// O(1) instead of running a full db.ListSites/GetVerification scan on every fetch
	// (findings #1/#2/#6/#12). It is refreshed at the SAME points installThrottleFloors
	// runs (initial startup below, reload, and after a demoting re-verify pass), so the
	// cached trust signal stays coherent with the throttle floors without a per-fetch DB
	// query. The first refresh happens alongside the startup floor install, before the
	// scheduler dispatches any fetch.
	verifiedHosts := newVerifiedSnapshot()
	uaFunc := perHostUserAgentFunc(&cfgMu, &cfg, verifiedHosts, opts.Version)
	// The robots client must enforce the same SSRF deny-list as the page fetcher:
	// its URL is derived from the (operator-supplied) site base URL, so a base URL
	// pointing at an internal/loopback/link-local/metadata host would otherwise
	// reach instance metadata or internal services via /robots.txt. GuardedClient
	// installs the fetcher's dial Control hook so both paths share one guard. The
	// per-host UA func is installed before any fetch so robots.txt requests carry
	// the same per-site identity as the page/sitemap fetches.
	robots := frontier.NewRobotsCache(fetcher.GuardedClient(30*time.Second), ua, 5*time.Minute)
	robots.SetUserAgentFunc(uaFunc)
	// Per-host politeness base: derived from the configured defaults (not hardcoded)
	// so a tuned defaults.per_host_rate/per_host_concurrency is honored as the
	// un-set-host base. Per-site effective rates still arrive via installThrottleFloors
	// (SetHostRate); the adaptive throttle backs off further on slow/erroring hosts.
	// This is the host kindness floor, distinct from the per-URL recheck cadence
	// (min/max interval), which governs how often a URL becomes due, not request spacing.
	baseRate, baseConc := frontierBaseFromConfig(&cfg)
	front := frontier.New(frontier.Options{PerHostRate: baseRate, PerHostConcurrency: baseConc})
	fetch := fetcher.New(fetcher.Options{UserAgent: ua, UserAgentFunc: uaFunc, Timeout: 30 * time.Second, MaxBodyBytes: 5 << 20, AllowPrivate: opts.AllowPrivate})
	ext := extract.NewExtractor()

	// Sitemap fetcher: larger body limit (50 MB) for sitemap indexes and compressed
	// sitemaps that can be substantially bigger than normal pages. Uses the same
	// SSRF-guarded dial as the page fetcher (GuardedClient is not used here; instead
	// the standard fetcher Options path enforces the deny-list at dial time) and the
	// same per-host UA func.
	sitemapFetch := fetcher.New(fetcher.Options{UserAgent: ua, UserAgentFunc: uaFunc, Timeout: 30 * time.Second, MaxBodyBytes: 50 << 20, AllowPrivate: opts.AllowPrivate})

	// A7 segment registry: the in-memory, hot-path classifier shared by the alert
	// pipeline (SegmentsFor annotation) and discovery (Classify seam). It is
	// rebuilt + atomically swapped by every reconcileSites pass (startup, reload,
	// post-reverify), so segment edits take effect with no daemon restart. Empty
	// until the first reconcile populates it.
	segReg := segments.NewRegistry()

	disc := &discovery.Discoverer{
		Store:    db,
		Pages:    fetch,
		Sitemaps: sitemapFetch,
		Robots:   robots,
		// A7: classify each NEWLY admitted URL into its segments at entry, reading
		// the in-memory registry (no DB read on the classify decision) and writing
		// the url_segments rows for this URL. A nil-matcher site or a URL in no
		// segment writes an empty membership set (idempotent). The error is returned
		// to the discoverer, which logs it non-fatally — reconcile re-classifies.
		Classify: func(cctx context.Context, siteID, urlID int64, rawURL string) error {
			return db.SetURLSegments(cctx, urlID, segReg.SegmentIDsFor(siteID, rawURL))
		},
		// Gate each sitemap fetch through the daemon's per-host rate + concurrency
		// budget (same frontier the page-crawl path uses) so a BFS over a site's
		// declared sitemaps cannot issue up to maxSitemapFetches ungated requests to
		// one host, ignoring its crawl-delay / per-host rate.
		Frontier: front,
		// Resolve against the LIVE cfg (locked per call), not a startup copy, so a
		// SIGHUP reload and runtime-added sites get correct per-site caps. The
		// getStateFn reads the authoritative DB proof state under the daemon ctx so
		// the page budget is clamped for unverified sites (verification-aware throttle).
		Resolve: discoveryResolver(&cfgMu, &cfg, func(siteID int64) verify.State {
			return verificationState(ctx, db, siteID)
		}),
		Now:    func() time.Time { return time.Now().UTC() },
		Logger: logger,
	}

	// ── M2 alerting stack ───────────────────────────────────────────────────
	// Build the notify registry, alerts pipeline (incident state machine), rules
	// engine, and post-fetch processor from config + the store. A misconfigured
	// notifier type is a startup error (unless ctx was cancelled mid-startup).
	// The Slack webhook client posts to operator-supplied https://hooks.slack.com
	// URLs (trusted config, not crawl targets), so it is a plain timeout client —
	// NOT the SSRF-guarded crawl client, which would reject those public hosts'
	// CDN-fronted IPs inconsistently and has no bearing on operator-chosen routes.
	nowUTC := func() time.Time { return time.Now().UTC() }
	// ── A9 link-graph LITE wiring (scope-gated) ──────────────────────────────
	// graph.enabled is the master switch. When ON we build the link-graph stack:
	//   1. a sink-less Grapher's BlastRadius lookup threaded into the Processor so a
	//      critical http_status (>=400) alert gains "linked from N pages (M high-
	//      importance)" — reads only the store, so it is built BEFORE the stack;
	//   2. a FULL Grapher (with the alerts pipeline as its incident sink + the
	//      config-sourced caps) assigned to Crawler.Graph for incremental edge sync
	//      and the page_orphaned / inlink_loss signals on the crawl path (below);
	//   3. a gocron click-depth BFS sweep beside the retention sweep (below).
	// graph.enabled=false leaves Crawler.Graph nil, registers no sweep, and passes
	// no blast-radius option — the scope-gate severability (a no-wiring decision,
	// nothing to revert).
	graphEnabled := cfg.Graph.Enabled
	stackOpts := []supervisor.StackOption{supervisor.WithStackMetrics(metrics)}
	if graphEnabled {
		// A read-only Grapher (no sink) backs the alert-enrichment lookup; it shares
		// the daemon's store and only issues a bounded indexed BlastRadius query per
		// >=400 alert. It is built BEFORE the stack because the Processor (which
		// consumes it) is constructed inside BuildAlertingStack.
		brGrapher := linkgraph.NewGrapher(db)
		stackOpts = append(stackOpts, supervisor.WithStackBlastRadius(brGrapher.BlastRadius))
	}
	stack, err := supervisor.BuildAlertingStack(cfg, db, &http.Client{Timeout: 30 * time.Second}, nowUTC, logger, segReg.SegmentsFor, stackOpts...)
	if err != nil {
		if ctx.Err() != nil {
			logger.Info("daemon stopped", obs.KeyComponent, "supervisor")
			return nil
		}
		return err
	}

	// The FULL crawl-hook Grapher: it owns the alerts pipeline as its incident sink
	// (so page_orphaned / inlink_loss / click_depth_regression reach Slack via the
	// same pipeline as every other event) and the config-sourced caps. It is nil when
	// graph.enabled=false, which leaves Crawler.Graph nil (feature off). The same
	// instance backs the gocron sweep below, so the in-process inlink high-water
	// baseline is shared between the crawl path and the sweep.
	var graphSyncer *linkgraph.Grapher
	if graphEnabled {
		graphSyncer = linkgraph.NewGrapher(db,
			linkgraph.WithAlertSink(stack.Pipeline),
			linkgraph.WithClock(nowUTC),
			linkgraph.WithMaxOutlinks(cfg.Graph.MaxOutlinksPerPage),
			linkgraph.WithExportCaps(cfg.Graph.ExportMaxNodes, cfg.Graph.ExportMaxEdges),
		)
	}

	// The A9 control read hooks (GET /v1/links, /v1/graph) backing the MCP tools.
	// They stay nil when graph is disabled so those routes return 501 — the
	// severability the scope gate requires (the MCP child sees the tools error as
	// data). The hooks construct their own sink-less Grapher (read-only); they thread
	// the startup graph caps (the routes are registered once, so they reflect the
	// startup graph.enabled like every other startup-bound surface).
	var linksHookFn func(ctx context.Context, url string, limit int) (control.LinksResponse, error)
	var graphHookFn func(ctx context.Context, q control.GraphQuery) (control.GraphResponse, bool, error)
	if graphEnabled {
		linksHookFn = linksHook(db, cfg.Graph)
		graphHookFn = graphHook(db, cfg.Graph)
	}

	crawler := &scheduler.Crawler{
		Store:      db, // *store.DB now implements RecordChanges + GetSite (M2)
		Fetcher:    fetch,
		Extractor:  ext,
		Robots:     robots,
		Frontier:   front,
		Processor:  stack.Processor, // M2: diff -> rules -> alerts after each fetch
		Discoverer: disc,            // bounded link-following after each successful extract
		Logger:     logger,
		// B2: thread the self-observability layer through the crawl choke point so
		// rabbot_fetches_total{class} + rabbot_fetch_duration_seconds increment on a
		// real daemon (CrawlOne.ObserveFetch). Without this the field is nil and those
		// two families stay permanently zero in production. A nil *Metrics no-ops, so
		// M1-only paths and unit tests are unaffected.
		Metrics: metrics,
		// A8: thread the hydration-recovery knobs (crawler.hydration.*) into the
		// extractor so a hydrated/thin-DOM page recovers SEO signals from its
		// embedded framework state at crawl time. Enabled=true by default (config
		// Defaults); recovery happens from the in-memory body — no extra DB or HTTP.
		Hydration: extract.HydrationOptions{
			Enabled:         cfg.Crawler.Hydration.Enabled,
			MaxPayloadBytes: cfg.Crawler.Hydration.MaxPayloadBytes,
		},
	}
	// A9: assign the link-graph syncer ONLY when graph is enabled. A nil
	// *linkgraph.Grapher must NOT be stored in the Crawler.Graph interface — that
	// would make the interface non-nil (holding a nil pointer) and defeat the
	// nil-hook guard in CrawlOne. Setting the field only on the enabled path keeps
	// the interface genuinely nil when the feature is off (scope-gate severability).
	if graphSyncer != nil {
		crawler.Graph = graphSyncer
	}
	sched := &scheduler.Scheduler{
		DueStore:    db,
		CrawlFunc:   crawler.CrawlOne,
		Batch:       50,
		MinInterval: int64(cfg.MinIntervalDuration().Seconds()),
		MaxInterval: int64(cfg.MaxIntervalDuration().Seconds()),
		MaxParallel: 8,
		SelectorFor: func(model.URL) string { return "" },
		Log:         logger,
	}
	// rabbot_crawls_in_flight reads the scheduler's atomic QueueDepth on every
	// scrape — an atomic load, safe on the scrape path and never touching the DB.
	metrics.SetInFlightFunc(sched.QueueDepth)
	// The robots side-timer feeds detected robots.txt changes into the SAME alerts
	// pipeline as the per-URL change stream (#8): a deploy shipping "Disallow: /"
	// must raise a Slack alert. BuildAlertingStack always returns a live pipeline
	// (above), so Alerts is non-nil here; the pipeline self-suppresses delivery when
	// no notifier route matches, so this is a no-op when alerting is unconfigured.
	// The sitemap side-timer (A2) reuses the same alerts pipeline: a sitemap that
	// breaks (status regression), a declared-URL-set change, or growing coverage
	// drift each ingest a site-level event. Sitemaps runs the bounded collection +
	// admission pass (discovery.Discoverer); URLStore reconciles urls.in_sitemap and
	// reads the live coverage counts (adapter bridges the store↔scheduler seam).
	side := &scheduler.SideTimers{
		FileStore: db,
		Robots:    robots,
		Sitemaps:  disc,
		URLStore:  supervisor.SitemapURLStore{DB: db},
		Alerts:    stack.Pipeline,
	}

	// GSC W1 puller: built only when at least one site has a `gsc` block (nil
	// otherwise → registerGSCPull registers nothing, the feature is off). It reads
	// the per-site GSC config through a cfgMu-guarded resolver so a live reload
	// cannot race the pull. It is registered on the gocron scheduler below beside the
	// other periodic jobs; daemon shutdown cancels its ctx and gsched.Shutdown stops
	// it. The puller stores GSC ground truth (search_metrics / url_index_status) —
	// the signals/rules over those rows are a separate (W2) concern.
	gscPuller := buildGSCPuller(&cfgMu, &cfg, db, logger)
	// GSC W2 signal evaluator (index_status_discrepancy / google_canonical_mismatch),
	// built ONLY when the puller exists (a GSC-configured site) so it shares the same
	// gate. It holds the live alert pipeline as its sink, so a discrepancy emits/
	// resolves through the SAME incident machinery the crawl change-stream uses. The
	// pull job (registerGSCPull) calls Evaluate after each successful per-site Pull;
	// a nil evaluator (no GSC site) makes that call a safe no-op.
	var gscSignals *scheduler.GSCSignals
	if gscPuller != nil {
		gscSignals = buildGSCSignals(db, stack.Pipeline)
	}

	d := &supervisor.Daemon{
		Logger:       logger,
		TickInterval: opts.TickInterval,
	}

	// reload re-reads config.yaml and re-syncs it into the DB (decision S1). It is
	// invoked on SIGHUP / control /v1/reload and after every config-mutating hook
	// (sites add/remove, config set).
	reload := func() error {
		cfgMu.Lock()
		if opts.ConfigPath != "" {
			c, lerr := config.Load(opts.ConfigPath, nil)
			if lerr != nil {
				cfgMu.Unlock()
				return lerr
			}
			cfg = c
		}
		snapshot := cfg
		cfgMu.Unlock()
		logger.Info("reload requested", obs.KeyComponent, "supervisor")
		if rerr := reconcileSites(ctx, db, &snapshot, opts.Version, fetch, disc, time.Now().UTC(), logger, segReg); rerr != nil {
			return rerr
		}
		// Install per-host spacing floors for throttled (unverified) sites so a
		// newly-added/re-enabled unverified host crawls politely from its first
		// request. Best-effort and only-raises; verified sites install no floor.
		installThrottleFloors(ctx, db, &snapshot, front, logger)
		// Refresh the hot-path verified-host cache on the SAME pass so a newly added,
		// re-enabled, or verification-changed site is reflected in the per-fetch UA
		// trust signal without a DB query on the crawl path.
		verifiedHosts.refresh(ctx, db)
		return nil
	}
	d.OnReload = reload

	// Initial sync at startup (best-effort: a sync failure must not prevent the
	// daemon from starting and serving control requests).
	cfgMu.Lock()
	initialCfg := cfg
	cfgMu.Unlock()
	if serr := reconcileSites(ctx, db, &initialCfg, opts.Version, fetch, disc, time.Now().UTC(), logger, segReg); serr != nil && ctx.Err() == nil {
		logger.Error("initial site sync failed", obs.KeyComponent, "supervisor", obs.KeyError, serr.Error())
	}
	// Install the per-host throttle floor for throttled sites BEFORE the scheduler
	// loop starts, so an unverified host's very first crawl is already polite. This
	// is the ONE synchronous floor install (PR31 #4): the startup re-verify below
	// runs in the background and re-applies floors only if it demotes something, so
	// the standalone floor install that previously followed it was redundant.
	installThrottleFloors(ctx, db, &initialCfg, front, logger)
	// Build the hot-path verified-host cache on the SAME startup pass, BEFORE the
	// scheduler loop starts, so the very first fetch's UA reads an O(1) snapshot
	// rather than a per-fetch DB scan (findings #1/#2/#6/#12). Refreshed again on
	// reload and after a demoting re-verify pass.
	verifiedHosts.refresh(ctx, db)

	// pipelineWG joins every background goroutine that touches the store, so the
	// daemon does not Checkpoint/Close the DB while a scheduler tick, robots
	// refresh, or in-flight crawl write is still running. The scheduler loop joins
	// its own per-tick crawl goroutines inside Tick (wg.Wait), so waiting on the
	// loop goroutine transitively covers the crawl goroutines too.
	var pipelineWG sync.WaitGroup

	// Scheduler loop: pops due URLs and dispatches them through the crawl pipeline.
	pipelineWG.Add(1)
	go func() {
		defer pipelineWG.Done()
		if e := sched.Run(ctx, opts.TickInterval); e != nil && ctx.Err() == nil {
			logger.Error("scheduler stopped", obs.KeyComponent, "scheduler", obs.KeyError, e.Error())
		}
	}()

	// Metrics sampler: refreshes the DB-backed gauges (rabbot_due_urls,
	// rabbot_db_size_bytes) on a slow timer, OFF the scrape path so the scrape
	// handler never touches the store — no writer stalls, no teardown race. It is
	// ctx-tied and pipelineWG-joined like every other store-touching background
	// goroutine, so cancel stops it and pipelineWG.Wait drains it before the DB
	// Checkpoint/Close. It runs even when metrics are off (a nil-safe Set no-ops),
	// so the gauges are warm the moment a listener is later added — and it stays a
	// single, simple lifecycle with no branch on metrics.addr.
	dbPath := filepath.Join(opts.DataDir, "rabbot.db")
	pipelineWG.Add(1)
	go func() {
		defer pipelineWG.Done()
		t := time.NewTicker(metricsSampleInterval)
		defer t.Stop()
		runMetricsSampler(ctx, metrics, db, dbPath, t.C)
	}()

	// Startup re-verify, in the BACKGROUND (PR31 #1). A token that vanished while
	// the daemon was down is caught immediately — a verified site whose proof
	// disappeared demotes to throttled (it only DEMOTES; a throttled site is lifted
	// only by an explicit `rabbot verify`). This issues live HTTPS GETs (up to
	// 15s each, sequentially) per verified site, so running it on the main startup
	// path blocked the control-server bind and the scheduler start for up to N×15s.
	// Moving it into a pipelineWG-joined, ctx-cancellable goroutine lets bind +
	// scheduler start immediately while the daemon is still drained on shutdown
	// (cancel stops it, pipelineWG.Wait joins it before the DB Checkpoint/Close).
	//
	// It mirrors the periodic gate (PR31 #2): reconcileAfterReverify — which is
	// destructive to scheduling — runs ONLY when the pass actually demoted a site,
	// re-seeding that site's urls.interval through the verification-aware resolver
	// (now reading the freshly-demoted StateThrottled proof) and re-applying the
	// per-host throttle floor on the same pass.
	pipelineWG.Add(1)
	go func() {
		defer pipelineWG.Done()
		demoted, rerr := reverifyAll(ctx, db, verify.Verify, instKey, time.Now().UTC(), logger)
		if rerr != nil && ctx.Err() == nil {
			logger.Debug("startup re-verify failed", obs.KeyComponent, "supervisor", obs.KeyError, rerr.Error())
		}
		if demoted > 0 && ctx.Err() == nil {
			cfgMu.Lock()
			snapshot := cfg
			cfgMu.Unlock()
			reconcileAfterReverify(ctx, db, &snapshot, opts.Version, fetch, disc, front, logger, segReg)
			// A demoting re-verify flipped at least one site to throttled; refresh the
			// hot-path verified-host cache so the per-fetch UA trust signal reflects the
			// demotion without a DB query on the crawl path (findings #1/#2/#6/#12).
			verifiedHosts.refresh(ctx, db)
		}
	}()

	// SIGHUP reloads config without a restart — Unix operators expect `kill -HUP`,
	// and reload is documented as a SIGHUP trigger. (SIGINT/SIGTERM shut down via
	// the signal.NotifyContext above; HUP must NOT go through that, which cancels.)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	pipelineWG.Add(1)
	go func() {
		defer pipelineWG.Done()
		defer signal.Stop(hup)
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if rerr := reload(); rerr != nil && ctx.Err() == nil {
					logger.Error("SIGHUP reload failed", obs.KeyComponent, "supervisor", obs.KeyError, rerr.Error())
				}
			}
		}
	}()

	// Robots side-timer: every 5 minutes, refresh robots.txt for each enabled site
	// and persist a file_snapshot. Harmless with zero sites; ctx-tied and non-fatal.
	//
	// It also PIGGYBACKS the periodic re-verify of the living verification state:
	// every reverifyEveryNTicks ticks (~hourly) it re-runs reverifyAll, which is a
	// single bounded token fetch per verified site (no new timer, no new goroutine,
	// negligible added load). This runs inside the pipelineWG-joined robots
	// goroutine, so ctx cancel stops it and pipelineWG.Wait drains it before the DB
	// Checkpoint/Close. After a re-verify pass it re-installs the per-host spacing
	// floors so a freshly-demoted site is throttled immediately.
	const reverifyEveryNTicks = 12 // 12 * 5m = ~1h
	pipelineWG.Add(1)
	go func() {
		defer pipelineWG.Done()
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		ticks := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sites, lerr := db.ListSites(ctx)
				if lerr != nil {
					if ctx.Err() == nil {
						logger.Debug("robots refresh: list sites failed", obs.KeyComponent, "supervisor", obs.KeyError, lerr.Error())
					}
					continue
				}
				for _, s := range sites {
					if !s.Enabled {
						continue
					}
					if rerr := side.RefreshRobots(ctx, s.ID, s.BaseURL); rerr != nil && ctx.Err() == nil {
						logger.Debug("robots refresh failed", obs.KeyComponent, "supervisor",
							"site", s.BaseURL, obs.KeyError, rerr.Error())
					}
				}

				// Periodic living-state re-verify on the same cadence (~hourly).
				ticks++
				if ticks%reverifyEveryNTicks == 0 {
					demoted, rerr := reverifyAll(ctx, db, verify.Verify, instKey, time.Now().UTC(), logger)
					if rerr != nil && ctx.Err() == nil {
						logger.Debug("periodic re-verify failed", obs.KeyComponent, "supervisor", obs.KeyError, rerr.Error())
					}
					// Reconcile ONLY when a site actually demoted this pass (PR31 #2).
					// reconcileAfterReverify is destructive to scheduling: reconcileSites
					// re-seeds each site's base URL due-now at the resolved minInterval,
					// discarding the adaptively-grown urls.interval/next_check_at. Running
					// it unconditionally every ~1h reset every homepage's schedule even
					// when no verification state changed. A demotion only writes the proof
					// record (reverifyAll never touches SetSiteThrottle/urls.interval), so
					// when demoted > 0 the reconcile re-seeds the demoted site's
					// urls.interval through the verification-aware resolver (now reading the
					// freshly-demoted StateThrottled proof) and re-applies the per-host
					// throttle floor on the same pass. Best-effort, matching the side-timer
					// style.
					if demoted > 0 {
						cfgMu.Lock()
						snapshot := cfg
						cfgMu.Unlock()
						reconcileAfterReverify(ctx, db, &snapshot, opts.Version, fetch, disc, front, logger, segReg)
						// A demoting re-verify flipped at least one site to throttled;
						// refresh the hot-path verified-host cache so the per-fetch UA
						// trust signal reflects the demotion without a DB query on the
						// crawl path (findings #1/#2/#6/#12).
						verifiedHosts.refresh(ctx, db)
					}
				}
			}
		}
	}()

	// Sitemap-refresh ticker: periodically runs the sitemap WATCH (A2) for each
	// enabled site — one collection pass that admits newly published sitemap entries
	// (e.g. from a fresh blog post) AND snapshots/diffs/reconciles the declared set,
	// so a broken sitemap, a URL-set change, or growing coverage drift alerts without
	// waiting for an operator reload. The single collection pass replaces the prior
	// additive-only SeedSitemaps call: discovery admission still happens, inside it.
	//
	// The cadence is the GLOBAL default SitemapRefresh (24h unless overridden),
	// re-read under cfgMu on every tick so a SIGHUP reload that changes
	// defaults.discovery.sitemap_refresh takes effect without a restart. NOTE:
	// per-site sitemap_refresh is not yet honored — the ticker fires globally and
	// refreshes every enabled site on the one shared cadence.
	pipelineWG.Add(1)
	go func() {
		defer pipelineWG.Done()
		// liveCadence snapshots the current global sitemap-refresh under cfgMu.
		liveCadence := func() time.Duration {
			cfgMu.Lock()
			d := cfg.ResolveDiscovery(config.SiteConfig{}).SitemapRefresh
			cfgMu.Unlock()
			return d
		}
		// refreshAll runs ONE sitemap WATCH pass over every enabled site: the exact
		// per-tick body, factored out so it can also be fired EAGERLY at startup. It
		// is best-effort and ctx-aware — a list/refresh error is logged at Debug and
		// never aborts (matching the side-timer style), and RefreshSitemap itself
		// preserves the incomplete-collection / no-prior-snapshot guard
		// (sidetimers_sitemap.go), so the eager pass never persists an empty or
		// partial first snapshot.
		refreshAll := func() {
			sites, lerr := db.ListSites(ctx)
			if lerr != nil {
				if ctx.Err() == nil {
					logger.Debug("sitemap refresh: list sites failed", obs.KeyComponent, "supervisor", obs.KeyError, lerr.Error())
				}
				return
			}
			for _, s := range sites {
				if !s.Enabled {
					continue
				}
				if serr := side.RefreshSitemap(ctx, s); serr != nil && ctx.Err() == nil {
					logger.Debug("sitemap refresh error", obs.KeyComponent, "supervisor", "site", s.BaseURL, obs.KeyError, serr.Error())
				}
			}
		}
		// Eager first pass (issue #88): a time.Ticker's first tick fires a full
		// interval later, so without this the FileKindSitemap snapshot that backs
		// get_coverage's has_sitemap would not land until the first 24h tick —
		// leaving coverage dark for ~24h on every freshly-added site even when it
		// correctly declares a sitemap. Fire one pass now so the snapshot lands on
		// the first crawl cycle; the periodic ticker below then continues unchanged.
		refreshAll()
		interval := liveCadence()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Re-read the cadence LIVE; a reload may have changed it. Reset the
				// ticker so the new interval governs the next fire.
				if cur := liveCadence(); cur != interval {
					interval = cur
					t.Reset(interval)
				}
				refreshAll()
			}
		}
	}()

	// ── M2 side timers (gocron) ─────────────────────────────────────────────
	// The alerts pipeline registers its 24h incident auto-close sweep and the
	// periodic digest-flush job onto a gocron scheduler whose Start/Shutdown
	// lifecycle the daemon owns. Init failure is non-fatal: the daemon still
	// crawls and alerts; only the periodic sweeps are skipped (stale incidents
	// close on the next recovery or a later restart, and throttle-deferred
	// non-critical alerts stay buffered until the next flush or restart). The
	// digest job drains the supervisor buffer (over-cap / non-critical alerts)
	// and dispatches each via the local dispatcher; criticals always dispatch
	// immediately on the fetch path regardless.
	gsched, gerr := gocron.NewScheduler()
	if gerr != nil {
		logger.Error("gocron init failed; incident auto-close + digest sweeps disabled",
			obs.KeyComponent, "alerts", obs.KeyError, gerr.Error())
		gsched = nil
	} else {
		// Honor alerting.digest.schedule (F15): a non-positive/unparseable value
		// makes RegisterTimers fall back to its 1h default.
		digestInterval, derr := time.ParseDuration(cfg.Alerting.Digest.Schedule)
		if derr != nil {
			digestInterval = 0 // let RegisterTimers apply its 1h fallback
		}
		if rerr := stack.Pipeline.RegisterTimers(ctx, logger, gsched, db, digestInterval, func() {
			if stack.DigestFlush != nil {
				stack.DigestFlush(ctx)
			}
		}); rerr != nil {
			logger.Error("register alert timers failed", obs.KeyComponent, "alerts", obs.KeyError, rerr.Error())
		}
		if _, rerr := registerRetentionSweep(ctx, logger, gsched, db, &cfg); rerr != nil {
			logger.Error("register retention sweep failed", obs.KeyComponent, "retention", obs.KeyError, rerr.Error())
		}
		// A9: the click-depth BFS sweep beside the retention sweep. It runs only when
		// graph is enabled (graphSyncer non-nil) and shares that one Grapher instance
		// so the crawl path and the sweep agree on the inlink high-water baseline.
		if _, rerr := registerGraphSweep(ctx, logger, gsched, db, graphSyncer, cfg.GraphSweepInterval()); rerr != nil {
			logger.Error("register graph sweep failed", obs.KeyComponent, "linkgraph", obs.KeyError, rerr.Error())
		}
		// GSC W1: the daily Search Console pull (search-analytics + bounded URL
		// inspection). Registered only when gscPuller is non-nil (a GSC-configured
		// site). SingletonMode prevents an overlapping pull on a slow property; init
		// failure is non-fatal, matching the sibling jobs.
		if _, rerr := registerGSCPull(ctx, logger, gsched, db, gscPuller, gscSignals, gscPullInterval); rerr != nil {
			logger.Error("register gsc pull failed", obs.KeyComponent, "gsc", obs.KeyError, rerr.Error())
		}
		gsched.Start()
	}

	// ── Metrics listener (B2) ───────────────────────────────────────────────
	// A SEPARATE, read-only, GET-only, UNAUTHENTICATED server — deliberately not
	// part of internal/control (which is loopback + token with mutation hooks).
	// Off by default: opts.MetricsAddr == "" opens no listener. When set, it binds
	// mirroring the control-server bind-failure pattern (F18): a bind failure is a
	// FATAL startup error — a configured-but-unbindable observability surface must
	// fail loudly, not run silently without it. A non-loopback bind logs a startup
	// Warn (unauthenticated read-only surface). The serve goroutine is
	// pipelineWG-joined; the listener is shut down in teardown step (1) alongside
	// the control server.
	var metricsSrv *obs.MetricsServer
	metricsServeErr := make(chan error, 1)
	var metricsBindErr error
	if opts.MetricsAddr != "" {
		if !isLoopbackAddr(opts.MetricsAddr) {
			logger.Warn("metrics listener bound to a non-loopback address (unauthenticated, read-only)",
				obs.KeyComponent, "metrics", "addr", opts.MetricsAddr)
		}
		metricsSrv = obs.NewMetricsServer(metrics)
		pipelineWG.Add(1)
		go func() {
			defer pipelineWG.Done()
			defer close(metricsServeErr)
			// A clean shutdown returns http.ErrServerClosed; that is expected and must
			// not be reported as a fatal bind error (mirrors the control server, F35).
			if serveErr := metricsSrv.ListenAndServe(opts.MetricsAddr); serveErr != nil &&
				!errors.Is(serveErr, http.ErrServerClosed) {
				metricsServeErr <- serveErr
			}
		}()
		// Wait briefly for a bind failure to surface (net.Listen is synchronous, so a
		// bind error arrives near-instantly), distinguishing "failed to bind" from
		// "bound and serving". Mirrors the control-server grace-select below.
		mGrace := time.NewTimer(100 * time.Millisecond)
		select {
		case serveErr := <-metricsServeErr:
			mGrace.Stop()
			if serveErr != nil && ctx.Err() == nil {
				logger.Error("metrics listener bind failed", obs.KeyComponent, "metrics", obs.KeyError, serveErr.Error())
				metricsBindErr = serveErr
			}
		case <-mGrace.C:
			// No early error: the listener bound and is serving.
			logger.Info("metrics listener serving", obs.KeyComponent, "metrics", "addr", opts.MetricsAddr)
		}
	}

	if metricsBindErr != nil {
		// Tear down everything started so far, then fail (same shape as the control
		// bind-failure path below). Cancelling stops the scheduler/sampler/robots
		// loops; pipelineWG joins them and the (already-returned) metrics goroutine.
		cancel()
		if gsched != nil {
			_ = gsched.Shutdown()
		}
		pipelineWG.Wait()
		if cpErr := db.Checkpoint(context.Background()); cpErr != nil {
			logger.Error("checkpoint on shutdown failed", obs.KeyComponent, "store", obs.KeyError, cpErr.Error())
		}
		return metricsBindErr
	}

	var ctrlSrv *control.Server
	// ctrlServeErr carries the control server's terminal error (bind failure or an
	// unexpected Serve error; http.ErrServerClosed is filtered out as the expected
	// clean-shutdown sentinel — F35). ctrlBindErr records a fatal startup bind
	// failure so runDaemon aborts instead of running headless (F18).
	ctrlServeErr := make(chan error, 1)
	var ctrlBindErr error
	if opts.ControlPort > 0 {
		// egress is a best-effort one-shot lookup of the daemon's outbound IP(s),
		// surfaced in the richer Status hook. "" proxy = direct egress. The probe is
		// an opt-out (crawler.egress_check_enabled, default true): when disabled the
		// outbound call is skipped entirely and Status simply omits the egress IP.
		var egress fetcher.EgressInfo
		if opts.EgressCheckEnabled {
			egress, _ = fetcher.EgressIP(ctx, opts.EgressCheckEndpoint, "", false)
		}

		// Every M1 control hook is wired from the live daemon components. The
		// global pause flag lives on the frontier (the real crawl kill-switch);
		// Crawl/AddSite/RemoveSite/SetConfig drive the store + config-mutation
		// path; Reload re-syncs config->DB. IgnoreIssue/NotifyTest are wired below.
		hooks := supervisor.BuildControlHooks(supervisor.HookDeps{
			Reload: d.Reload,
			Pause: func(_ context.Context, p bool) error {
				action := "resume_monitoring"
				if p {
					action = "pause_monitoring"
				}
				return logMutation(logger, action, nil, func() error {
					front.SetPaused(p) // global crawl kill-switch
					return nil
				})
			},
			Crawl: func(hctx context.Context, req control.CrawlRequest) (control.CrawlResponse, error) {
				var resp control.CrawlResponse
				err := logMutation(logger, "recheck_site", map[string]any{obs.KeySite: req.Target}, func() error {
					n, cerr := db.EnqueueRecheck(hctx, req.Target, time.Now().UTC())
					resp = control.CrawlResponse{Queued: n}
					return cerr
				})
				return resp, err
			},
			AddSite: func(hctx context.Context, req control.AddSiteRequest) (control.AddSiteResponse, error) {
				var resp control.AddSiteResponse
				err := logMutation(logger, "add_site", map[string]any{obs.KeySite: req.URL}, func() error {
					// Serialize the duplicate pre-check + config.yaml write + Reload as one
					// critical section (High#1): without this, concurrent add_site calls
					// interleave their read-modify-write of config.yaml and lose most updates.
					// Held across the check-then-act window so a concurrent same-URL add
					// cannot slip between the GetSiteByBaseURL lookup and the AddSiteYAML write.
					cfgWriteMu.Lock()
					defer cfgWriteMu.Unlock()
					// Reject an unsafe base URL at admission (scheme + IP-literal range)
					// before it is ever written to config.yaml or used as a fetch target.
					// A bad URL is a caller fault -> control.ErrBadRequest (HTTP 400).
					if verr := addSiteURLError(req.URL, fetch.AllowsPrivate()); verr != nil {
						return verr
					}
					// Reject a duplicate (already-enabled) site BEFORE touching
					// config.yaml — otherwise the entry is silently appended a second time
					// and reconcile merely re-enables the existing row. A duplicate is a
					// caller fault (HTTP 400); a non-ErrNotFound lookup error is an internal
					// fault (HTTP 500). config.AddSiteYAML does not detect duplicates, so
					// this store pre-check is a best-effort admission gate for a clean 400.
					// The store's UNIQUE(base_url) index — now surfaced as ErrSiteExists —
					// is the authoritative duplicate guard that backstops the small
					// check-then-act window between this lookup and the write below.
					existing, lookupErr := db.GetSiteByBaseURL(hctx, req.URL)
					if derr := classifyAddSiteStoreErr(addSiteDuplicateErr(existing, lookupErr)); derr != nil {
						return derr
					}
					if aerr := config.AddSiteYAML(opts.ConfigPath, config.SiteConfig{
						URL:         req.URL,
						Name:        req.Name,
						MinInterval: req.MinInterval,
						MaxInterval: req.MaxInterval,
						Speed:       req.Speed,
					}); aerr != nil {
						return aerr
					}
					if rerr := d.Reload(); rerr != nil {
						return rerr
					}
					s, gerr := db.GetSiteByBaseURL(hctx, req.URL)
					if gerr != nil {
						return gerr
					}
					resp = control.AddSiteResponse{SiteID: s.ID}
					return nil
				})
				return resp, err
			},
			RemoveSite: func(hctx context.Context, id int64, purge bool) error {
				return logMutation(logger, "remove_site", map[string]any{obs.KeySiteID: id}, func() error {
					// Serialize the config.yaml read-modify-write + Reload with the other
					// mutating hooks (High#1) so a concurrent add/remove/set cannot lose
					// this removal's write (or have its own write lost by this one).
					cfgWriteMu.Lock()
					defer cfgWriteMu.Unlock()
					s, gerr := db.GetSite(hctx, id)
					if gerr != nil {
						return gerr
					}
					if _, rerr := config.RemoveSiteYAML(opts.ConfigPath, s.BaseURL); rerr != nil {
						return rerr
					}
					if rerr := d.Reload(); rerr != nil {
						return rerr
					}
					if purge {
						return db.DeleteSite(hctx, id)
					}
					return nil
				})
			},
			SetConfig: func(_ context.Context, req control.ConfigSetRequest) error {
				// Authoritative allow-list guard: reject any key not explicitly
				// settable over the control plane (and always reject the throttle
				// floor, notifier secrets, and the data location). This is the
				// single enforcement point shared by `rabbot config set` and the
				// MCP set_config tool — neither can weaken the floor. A rejected key
				// is NOT written and NOT reloaded. The error carries the key name and
				// the allowed list, never the value (which may be a secret).
				if gerr := config.AllowConfigKey(req.Key); gerr != nil {
					return gerr
				}
				// Log the key, never the value (secret-safety).
				return logMutation(logger, "set_config", map[string]any{"key": req.Key}, func() error {
					// Serialize the config.yaml read-modify-write + Reload with the other
					// mutating hooks (High#1): SetKeyYAML rewrites the whole file, so an
					// interleaved add/remove RMW would otherwise drop this key's write or
					// have its own site write dropped by this rewrite.
					cfgWriteMu.Lock()
					defer cfgWriteMu.Unlock()
					if serr := config.SetKeyYAML(opts.ConfigPath, req.Key, req.Value); serr != nil {
						return serr
					}
					return d.Reload()
				})
			},
			Status: func(hctx context.Context) (control.StatusResponse, error) {
				resp := statusFromStore(hctx, db, opts.Version, started)
				resp.Paused = front.Paused()
				resp.EgressIP = egress.IPs // fetcher.EgressInfo.IPs ([]string)
				if n, derr := db.CountDueURLs(hctx, time.Now().UTC()); derr == nil {
					resp.DueCount = n
				}
				resp.QueueDepth = sched.QueueDepth()
				resp.CappedSites = cappedSitesCount(hctx, db, newBaseURLCapResolver(&cfgMu, &cfg))
				// Surface whether the read-only /metrics listener is on and where, so
				// `rabbot status` (and MCP get_status, which embeds this response) reports
				// self-observability state. Empty when metrics are off (B2).
				resp.MetricsAddr = opts.MetricsAddr
				return resp, nil
			},
			// M2: mark an issue ignored (CLI `issue ignore`) via the store.
			IgnoreIssue: func(hctx context.Context, id int64) error {
				return logMutation(logger, "ignore_issue", map[string]any{"issue_id": id}, func() error {
					return db.IgnoreIssue(hctx, id)
				})
			},
			// M2: send a synthetic alert through a named notifier (CLI `notify
			// test`) so operators can verify Slack wiring without a real change.
			NotifyTest: func(hctx context.Context, name string) error {
				// Log the notifier name, never its URL (secret-safety).
				return logMutation(logger, "send_test_alert", map[string]any{obs.KeyNotifier: name}, func() error {
					n, ok := stack.Registry.Get(name)
					if !ok {
						return fmt.Errorf("rabbot: no notifier named %q", name)
					}
					return n.Notify(hctx, notify.Alert{
						Site:       "rabbot-test.example",
						URL:        "https://rabbot-test.example/",
						ChangeType: "notify_test",
						Severity:   model.SeverityInfo,
						Before:     "(test before)",
						After:      "(test after)",
						DetectedAt: time.Now().UTC(),
						DeepLink:   "https://rabbot-test.example/",
					})
				})
			},
			// Read endpoints (Spec 2 — full MCP connection): all server-side over
			// the live store, so the mcp child no longer opens the DB (D2).
			ListSites:   listSitesHook(db),
			SiteDetail:  siteDetailHook(db, siteCapResolver(&cfgMu, &cfg)),
			Issues:      issuesHook(db),
			History:     historyHook(db),
			Report:      reportHook(db),
			Coverage:    coverageHook(db),
			RichResults: richResultsHook(db),
			Score:       scoreHook(db),
			// GSC W2 read endpoints: latest index status + search performance for a
			// URL, served by the daemon's store (the mcp child opens no DB, D2).
			IndexStatus:       indexStatusHook(db),
			SearchPerformance: searchPerformanceHook(db),
			// A9 link-graph reads: nil when graph is disabled (route 501).
			Links: linksHookFn,
			Graph: graphHookFn,
			// POST /v1/verify: daemon-owned begin/check. The daemon holds the single
			// DB writer + the fetcher + the instance key, so the verify write never
			// races a CLI invocation (D6 single-writer).
			Verify: func(hctx context.Context, req control.VerifyRequest) (control.VerifyResponse, error) {
				return daemonVerify(hctx, db, instKey, req)
			},
		})
		ctrlSrv = control.NewServer(control.ServerOptions{
			Token:   opts.ControlToken,
			Version: opts.Version,
			Hooks:   hooks,
			// POST /v1/shutdown -> cancel the daemon's root ctx, which runs the same
			// graceful drain/checkpoint teardown the SIGINT/SIGTERM path uses (it tears
			// down the scheduler, control server, pipeline, then checkpoints+closes the
			// DB below). The handler writes its 202 response before this fires, so the
			// teardown of this very control server cannot race the reply (#43).
			Shutdown: cancel,
		})

		// The control server's ListenAndServe binds 127.0.0.1:<port> synchronously
		// before serving and returns the net.Listen error on a bind failure (e.g.
		// the port is already held by a stale daemon or another process). Surface
		// that error to runDaemon so it is fatal — a daemon that cannot be controlled
		// (pause/resume/status/crawl/reload/notify-test all fail) must not run on
		// headless instead of failing loudly (F18). The goroutine is joined by
		// pipelineWG so its store-touching control handlers are drained before the
		// DB checkpoint/close below.
		pipelineWG.Add(1)
		go func() {
			defer pipelineWG.Done()
			defer close(ctrlServeErr)
			// A clean shutdown returns http.ErrServerClosed; that is expected and
			// must not be reported as a fatal bind error (F35).
			if serveErr := ctrlSrv.ListenAndServe(opts.ControlPort); serveErr != nil &&
				!errors.Is(serveErr, http.ErrServerClosed) {
				ctrlServeErr <- serveErr
			}
		}()

		// Wait briefly for a bind failure to surface before continuing. A bind error
		// arrives near-instantly (net.Listen is synchronous), so a short grace window
		// distinguishes "failed to bind" from "bound and serving"; on bind failure
		// (ctx not cancelled) abort startup. Mirrors store.Open / BuildAlertingStack.
		graceTimer := time.NewTimer(100 * time.Millisecond)
		select {
		case serveErr := <-ctrlServeErr:
			graceTimer.Stop()
			if serveErr != nil && ctx.Err() == nil {
				logger.Error("control server bind failed", obs.KeyComponent, "control", obs.KeyError, serveErr.Error())
				ctrlBindErr = serveErr
			}
		case <-graceTimer.C:
			// No early error: the listener bound and is serving.
		}
	}

	if ctrlBindErr != nil {
		// Tear down everything started so far, then fail. Cancelling the local ctx
		// stops the scheduler/robots/SIGHUP loops; pipelineWG joins them and the
		// (already-returned) control goroutine before the checkpoint/close. The
		// metrics listener is NOT ctx-tied (its serve goroutine returns only on
		// Shutdown), so stop it explicitly here or pipelineWG.Wait would hang.
		cancel()
		if metricsSrv != nil {
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = metricsSrv.Shutdown(shutCtx)
			shutCancel()
		}
		if gsched != nil {
			_ = gsched.Shutdown()
		}
		pipelineWG.Wait()
		if cpErr := db.Checkpoint(context.Background()); cpErr != nil {
			logger.Error("checkpoint on shutdown failed", obs.KeyComponent, "store", obs.KeyError, cpErr.Error())
		}
		return ctrlBindErr
	}

	err = d.RunLoop(ctx)

	// Shut down in dependency order so nothing touches the store after teardown
	// begins. (1) Stop the control server FIRST and drain in-flight handlers — a
	// late ignore/notify/crawl request would otherwise call db.* concurrently with
	// the checkpoint/close below. (2) Stop the gocron timers (auto-close sweep /
	// digest flush). (3) Drain the pipeline goroutines (scheduler loop + robots
	// side-timer, transitively the per-tick crawls). Only then checkpoint + close.
	if ctrlSrv != nil {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = ctrlSrv.Shutdown(shutCtx)
		shutCancel()
	}
	// Stop the read-only metrics listener in the same teardown step (1) as the
	// control server: a late scrape touches only the registry (no DB), but stopping
	// it here keeps the network surface down before the pipeline drains and the DB
	// closes, and releases the port deterministically.
	if metricsSrv != nil {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = metricsSrv.Shutdown(shutCtx)
		shutCancel()
	}
	if gsched != nil {
		_ = gsched.Shutdown() // best-effort: teardown proceeds regardless of a timer-stop error
	}
	pipelineWG.Wait()

	// Surface an unexpected (non-ErrServerClosed) control-server Serve error that
	// occurred mid-run. Re-reading ctrlServeErr here is safe despite the earlier
	// receive in the startup grace-select (line ~574): only a non-nil (bind-
	// failure) receive there sets ctrlBindErr and aborts early via the return
	// above (L585), so it never reaches this drain — the two receives can't both consume
	// a real error (an early clean close just yields nil and falls through). The
	// cap-1 buffer lets the goroutine
	// send its terminal error and close without a reader, so this drain (after
	// pipelineWG.Wait) cannot deadlock; a clean shutdown sends nothing (the
	// goroutine filters http.ErrServerClosed, F35), so this stays silent.
	if ctrlSrv != nil {
		if serveErr := <-ctrlServeErr; serveErr != nil {
			logger.Error("control server stopped", obs.KeyComponent, "control", obs.KeyError, serveErr.Error())
		}
	}
	// Surface an unexpected (non-ErrServerClosed) metrics-listener Serve error. The
	// goroutine filters http.ErrServerClosed (clean shutdown) and the channel is
	// cap-1 and closed by the goroutine, so a clean shutdown sends nothing and this
	// stays silent; a real mid-run serve fault is logged. Only entered when a
	// listener was started (metricsSrv != nil), matching the bind path above.
	if metricsSrv != nil {
		if serveErr := <-metricsServeErr; serveErr != nil {
			logger.Error("metrics listener stopped", obs.KeyComponent, "metrics", obs.KeyError, serveErr.Error())
		}
	}

	if cpErr := db.Checkpoint(context.Background()); cpErr != nil {
		logger.Error("checkpoint on shutdown failed", obs.KeyComponent, "store", obs.KeyError, cpErr.Error())
	}
	logger.Info("daemon stopped", obs.KeyComponent, "supervisor")
	return err
}

// statusFromStore builds the minimal M0 StatusResponse. Counts are read from
// the store's read pool; a query error degrades to zero counts (status must
// never fail just because the tables are empty/new). due/queue depth are 0
// until M1's scheduler supplies the richer Status hook.
func statusFromStore(ctx context.Context, db *store.DB, version string, started time.Time) control.StatusResponse {
	resp := control.StatusResponse{
		Version: version,
		Uptime:  time.Since(started).Truncate(time.Second).String(),
		Paused:  false,
	}
	_ = db.Read().QueryRowContext(ctx, "SELECT COUNT(*) FROM sites").Scan(&resp.SiteCount)
	_ = db.Read().QueryRowContext(ctx, "SELECT COUNT(*) FROM urls").Scan(&resp.URLCount)
	if at, ok, lcErr := db.LastCrawlAt(ctx); lcErr == nil && ok {
		resp.LastCrawlAt = at.Format(time.RFC3339)
	} else {
		resp.LastCrawlAt = "never"
	}
	return resp
}

// ── AddSite error contract (finding #20.3) ──────────────────────────────────
// The control layer's handleAddSite maps a hook error that wraps
// control.ErrBadRequest (errors.Is) to HTTP 400 and everything else to 500. The
// production AddSite closure routes its caller-fault rejections through these
// small pure helpers so the translation is unit-testable without a live daemon,
// and so internal/control stays decoupled from internal/store/internal/fetcher.

// addSiteURLError validates rawURL as a safe outbound target (scheme + host +,
// unless allowPrivate, IP-literal range). A failure is always a caller fault, so
// it is wrapped with control.ErrBadRequest (-> HTTP 400). Returns nil when valid.
func addSiteURLError(rawURL string, allowPrivate bool) error {
	if verr := fetcher.ValidateSiteURL(rawURL, allowPrivate); verr != nil {
		return fmt.Errorf("%w: %w", control.ErrBadRequest, verr)
	}
	return nil
}

// addSiteDuplicateErr classifies the result of looking up an existing site by
// base URL during an add. An existing ENABLED row is a duplicate add and yields
// a store.ErrSiteExists error; a not-found row (store.ErrNotFound) and an
// existing DISABLED row are both legitimate (re)adds and yield nil — reconcile
// re-enables a disabled site, restoring it with its history. Any other lookup
// error is a genuine internal/IO fault and is returned unwrapped so the caller
// surfaces a 500. This is split from classifyAddSiteStoreErr so the duplicate
// decision is testable independently of the ErrBadRequest translation.
func addSiteDuplicateErr(existing model.Site, lookupErr error) error {
	switch {
	case lookupErr == nil:
		if existing.Enabled {
			return fmt.Errorf("add site %q: %w", existing.BaseURL, store.ErrSiteExists)
		}
		return nil
	case errors.Is(lookupErr, store.ErrNotFound):
		return nil
	default:
		return lookupErr
	}
}

// classifyAddSiteStoreErr translates a store-path error into the control error
// contract: a duplicate site (store.ErrSiteExists, at any wrap depth) is a
// client fault and is wrapped with control.ErrBadRequest (-> HTTP 400), while
// every other error — a real DB/IO fault — passes through unwrapped (-> HTTP
// 500). A nil error stays nil. Both sentinels are preserved (double %w) so a
// caller can still recover the specific cause via errors.Is.
func classifyAddSiteStoreErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrSiteExists) {
		return fmt.Errorf("%w: %w", control.ErrBadRequest, err)
	}
	return err
}

// reconcileAfterReverify widens the demoted sites' URL scheduling cadence after a
// periodic re-verify pass, mirroring what the startup path achieves. reverifyAll
// persists only the proof record on a demotion (it never touches SetSiteThrottle
// or urls.interval), so on its own a verified->throttled flip leaves each URL's
// interval at the verified tier — the scheduler would keep the demoted site due
// at its old (faster) cadence until a manual reload. reconcileSites re-seeds each
// site's urls.interval (and sites.min_interval) through the verification-aware
// resolver, which now reads the freshly-demoted StateThrottled proof, so the
// demoted site's per-URL cadence widens to the throttled MinInterval. The
// subsequent installThrottleFloors re-installs the per-host HTTP spacing floor so
// the 60s frontier protection lands on the same pass.
//
// Both calls are best-effort (log + continue), matching the side-timer style: a
// reconcile failure is logged and swallowed so it cannot stop the daemon, and the
// floor install is unconditional so the HTTP protection is never skipped on a
// reconcile error. ctx cancellation aborts both inner walks early.
func reconcileAfterReverify(ctx context.Context, db *store.DB, cfg *config.Config, version string, f fetcher.Fetcher, disc interface {
	SeedSitemaps(ctx context.Context, site model.Site) (int, error)
}, front *frontier.Frontier, logger *slog.Logger, reg *segments.Registry) {
	if rerr := reconcileSites(ctx, db, cfg, version, f, disc, time.Now().UTC(), logger, reg); rerr != nil && ctx.Err() == nil {
		logger.Debug("periodic re-verify reconcile failed", obs.KeyComponent, "supervisor", obs.KeyError, rerr.Error())
	}
	installThrottleFloors(ctx, db, cfg, front, logger)
}

// discoveryResolver maps a runtime site to its resolved discovery caps using the
// LIVE config (per-site override merged over defaults), keyed by base URL.
//
// It snapshots *cfg under cfgMu on EVERY call rather than closing over a startup
// value-copy: disc.Resolve is read concurrently from crawl goroutines and the
// sitemap ticker, and a SIGHUP reload (or a runtime-added site) reassigns cfg —
// so resolving against a stale copy would hand back the old/missing per-site
// caps. Taking the lock per call keeps every caller on the current config and
// races cleanly with reload's reassignment under the same mutex. The byURL map
// is rebuilt per call (cheap: a handful of sites) so a freshly-added site is
// visible the moment its config row exists.
// getStateFn reads a site's authoritative living verification state by site ID.
// Production wires it to the DB proof record (the closure captures the daemon
// ctx); tests inject a map so the resolver can be exercised without a store.
type getStateFn func(siteID int64) verify.State

func discoveryResolver(cfgMu *sync.Mutex, cfg *config.Config, getState getStateFn) func(model.Site) discovery.Caps {
	return func(site model.Site) discovery.Caps {
		cfgMu.Lock()
		snapshot := *cfg
		cfgMu.Unlock()
		byURL := make(map[string]config.SiteConfig, len(snapshot.Sites))
		for _, s := range snapshot.Sites {
			byURL[s.URL] = s
		}
		sc := byURL[site.BaseURL]
		rd := snapshot.ResolveDiscovery(sc)
		// Clamp the page budget by the verification-aware throttle. The cfg snapshot
		// is taken under cfgMu; the state read runs OUTSIDE the lock (the DB has its
		// own concurrency, and getState must not be held under cfgMu). A
		// never-verified site reads StateThrottled and its MaxPages is clamped to the
		// floor; FollowLinks/Sitemap/MaxDepth are unaffected by verification tier. The
		// getStateFn carries its own context (the daemon ctx in production), so the
		// fixed func(model.Site) Caps signature need not thread one through.
		eff := snapshot.ResolveCrawl(sc, getState(site.ID))
		return discovery.Caps{FollowLinks: rd.FollowLinks, Sitemap: rd.Sitemap, MaxDepth: rd.MaxDepth, MaxPages: eff.MaxPages}
	}
}

// perHostUserAgentFunc builds the daemon's per-host User-Agent closure. The daemon
// crawls many hosts through one shared fetcher, so it cannot bake a single static
// UA: it threads the per-site trust signal (verified-for / contact-unverified /
// confirm-or-block) at fetch time. The closure snapshots the LIVE config under
// cfgMu (so a SIGHUP reload of crawler.contact_email/user_agent is honored) and
// reads the per-host verification tier from the verifiedSnapshot — an O(1) map
// lookup, NO DB query on the fetch hot path (findings #1/#2/#6/#12: the fetcher
// invokes UserAgentFunc on EVERY page and sitemap request, so a per-call
// db.ListSites/GetVerification scan was a full-table scan per fetch). The snapshot
// is refreshed at the same points installThrottleFloors runs (startup, reload,
// post-re-verify); a miss/unknown host reads false, the cautious "unverified"
// default. The crawler.user_agent override (handled inside UserAgentFor) still wins
// verbatim.
func perHostUserAgentFunc(cfgMu *sync.Mutex, cfg *config.Config, snap *verifiedSnapshot, version string) func(host string) string {
	return func(host string) string {
		cfgMu.Lock()
		snapshot := *cfg
		cfgMu.Unlock()
		return snapshot.UserAgentFor(host, version, snap.verified(host))
	}
}

// verifiedSnapshot is a concurrency-safe host->verified cache read O(1) on the crawl
// hot path. It replaces the per-fetch db.ListSites + db.GetVerification scan that ran
// on EVERY page/sitemap request (findings #1/#2/#6/#12). It is rebuilt by refresh at
// the SAME points installThrottleFloors runs — startup, SIGHUP/control reload, and
// after a re-verify pass that demotes a site — so the cached tier never lags the
// authoritative proof state by more than one of those events. Keys are normalized to
// the bare lowercased hostname (port- and case-insensitive) so the host the fetcher
// passes (httpReq.URL.Hostname()) matches a site stored as host:port or with mixed
// case. The zero/never-refreshed snapshot reports everything unverified (fail-safe).
type verifiedSnapshot struct {
	mu sync.RWMutex
	m  map[string]bool
}

// newVerifiedSnapshot returns an empty snapshot (everything reads unverified until the
// first refresh). The daemon refreshes it during the same startup pass that installs
// the throttle floors, before the first fetch is dispatched.
func newVerifiedSnapshot() *verifiedSnapshot {
	return &verifiedSnapshot{m: map[string]bool{}}
}

// refresh rebuilds the host->verified map from the store: every ENABLED site's
// base-URL host (normalized) mapped to whether its live proof state is
// verify.StateVerified. It mirrors installThrottleFloors' host resolution (ListSites
// + verificationState). A list error leaves the previous snapshot intact (better a
// slightly stale cache than wiping every host to unverified on a transient glitch);
// ctx cancellation aborts the walk and also leaves the snapshot untouched.
func (s *verifiedSnapshot) refresh(ctx context.Context, db *store.DB) {
	sites, err := db.ListSites(ctx)
	if err != nil {
		return
	}
	next := make(map[string]bool, len(sites))
	for _, site := range sites {
		if ctx.Err() != nil {
			return
		}
		if !site.Enabled {
			continue
		}
		key := normalizeHost(hostFromURL(site.BaseURL))
		if key == "" {
			continue
		}
		next[key] = verificationState(ctx, db, site.ID) == verify.StateVerified
	}
	s.set(next)
}

// set swaps in a freshly-built host->verified map under the write lock. Exported to
// the package so tests can install a deterministic snapshot without a store.
func (s *verifiedSnapshot) set(m map[string]bool) {
	s.mu.Lock()
	s.m = m
	s.mu.Unlock()
}

// verified reports whether host is currently a verified site. It normalizes host the
// same way refresh normalizes the keys (bare lowercased hostname), so a host:port or
// mixed-case host the fetcher passes still matches. An unknown host yields false — the
// cautious "unverified" default that flags the crawl, never a false "verified".
func (s *verifiedSnapshot) verified(host string) bool {
	key := normalizeHost(host)
	if key == "" {
		return false
	}
	s.mu.RLock()
	v := s.m[key]
	s.mu.RUnlock()
	return v
}

// normalizeHost reduces a host[:port] value to its bare, lowercased hostname so both
// the snapshot keys and the fetcher-supplied host compare consistently. It mirrors
// url.URL.Hostname() (port stripping, IPv6 bracket unwrapping) then lowercases
// (DNS hostnames are case-insensitive). An empty input returns "".
func normalizeHost(host string) string {
	if host == "" {
		return ""
	}
	return strings.ToLower((&url.URL{Host: host}).Hostname())
}

// siteCapResolver builds the capResolver the site-detail hook uses to surface the
// per-site page cap. It snapshots live config under cfgMu (mirroring
// discoveryResolver) so a SIGHUP reload of defaults.discovery.max_pages_per_site
// is reflected without rebuilding the hook. The cap is the resolved discovery
// MaxPages (0 = unlimited); an unknown site (not in config) resolves against the
// defaults. The db param is unused for the cap (which is config-derived) but kept
// on the capResolver type so a future store-backed cap needs no signature change.
func siteCapResolver(cfgMu *sync.Mutex, cfg *config.Config) capResolver {
	return func(ctx context.Context, db *store.DB, siteID int64) int {
		site, err := db.GetSite(ctx, siteID)
		if err != nil {
			return 0
		}
		cfgMu.Lock()
		snapshot := *cfg
		cfgMu.Unlock()
		byURL := make(map[string]config.SiteConfig, len(snapshot.Sites))
		for _, s := range snapshot.Sites {
			byURL[s.URL] = s
		}
		return snapshot.ResolveDiscovery(byURL[site.BaseURL]).MaxPages
	}
}

// newBaseURLCapResolver builds the BaseURL-keyed page-cap resolver cappedSitesCount
// uses for the status hook. Unlike siteCapResolver it takes the base URL already
// present in the ListSites rows, so it needs no per-site GetSite round-trip (the
// cap is config-derived). It snapshots live config under cfgMu (mirroring
// siteCapResolver) so a SIGHUP reload of defaults.discovery.max_pages_per_site is
// reflected without rebuilding the hook; siteConfigCap does the URL->cap lookup.
func newBaseURLCapResolver(cfgMu *sync.Mutex, cfg *config.Config) baseURLCapResolver {
	return func(baseURL string) int {
		cfgMu.Lock()
		snapshot := *cfg
		cfgMu.Unlock()
		return siteConfigCap(&snapshot, baseURL)
	}
}
