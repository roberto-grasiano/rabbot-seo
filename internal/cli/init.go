package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/hostinfo"
	mcpsrv "github.com/roberto-grasiano/rabbot-seo/internal/mcp"
	"github.com/roberto-grasiano/rabbot-seo/internal/notify"
	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
	"github.com/roberto-grasiano/rabbot-seo/internal/setup"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
	"github.com/roberto-grasiano/rabbot-seo/internal/wizard"
)

// isTTY reports whether stdout is an interactive terminal. It is a package var so
// tests can override the routing decision deterministically (no real terminal in
// CI). Production uses go-isatty (covering Cygwin/mintty on Windows too).
var isTTY = func() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
}

// launchWizardFn is the seam through which init invokes the interactive wizard.
// Tests replace it with a stub to assert the routing decision without driving a
// real TUI; production points at launchWizard.
var launchWizardFn = launchWizard

// existingActionFn is the seam through which init presents the existing-config
// Add/Reconfigure/Cancel choice (spec step 1) on a TTY. Production points at
// promptExistingAction (a huh.Select); tests replace it to assert routing without
// a real terminal.
var existingActionFn = promptExistingAction

// setSiteVerificationFn is the seam for writing a per-site verification block.
// Production points at config.SetSiteVerificationYAML; tests override it to drive
// the found=false (silent-drop) path that the public wizard.Inputs seam cannot
// produce on its own, since setup.Apply always writes the sites we then look up.
var setSiteVerificationFn = config.SetSiteVerificationYAML

// sendTestAlertFn is the seam through which the alerts step (step 8) sends a
// best-effort test alert directly (no daemon). Production points at
// notify.SendTestAlert; tests replace it to avoid live network and to drive the
// advisory-failure path. The webhook value is never logged; any error it returns
// is already scrubbed of the URL by the underlying slackNotifier.
var sendTestAlertFn = notify.SendTestAlert

// offerSeeItWorkFn is the seam through which the post-go-live "see it work" step
// asks whether to show what the pipeline just read. Production points at
// offerSeeItWork (a huh.Confirm); tests replace it to assert the wiring without a
// real terminal. An abort or a declined confirm both yield false (skip).
var offerSeeItWorkFn = offerSeeItWork

// startDaemonFn is the seam for the run-now step (step 10): start the daemon
// after setup. Production points at startDaemonInProcess (exec `rabbot run`);
// tests replace it to assert routing without launching a real process.
var startDaemonFn = startDaemonInProcess

// installServiceFn is the seam for the install-service step (step 10). Production
// points at installServiceNow (kardianos newService + Install); tests replace it
// to assert routing without touching the host service manager.
var installServiceFn = installServiceNow

// sleepyHostFn is the seam for the wizard sleep-nudge (B7): true ⇒ the host looks
// like a machine that sleeps (a laptop), so the wizard prints one advisory line at
// go-live. Production points at hostinfo.Sleeper (a pure-Go, best-effort battery
// probe — false on any error/unknown platform); tests stub both arms. It is consulted
// ONLY on the wizard (TTY) path: the headless path never prints the nudge.
var sleepyHostFn = hostinfo.Sleeper

// maybePrintSleepNudge prints the wizard SleepNudge exactly once, and only when the
// host looks like a sleeper. It is the wizard-only go-live advisory (criterion 10):
// the headless flow never calls it, so scripts stay byte-stable. Best-effort and
// non-blocking — a non-sleeper host (or any probe error, surfaced as false) is silent.
func maybePrintSleepNudge(out io.Writer) {
	if sleepyHostFn() {
		_, _ = fmt.Fprintln(out, wizard.SleepNudge)
	}
}

// defaultConfigYAML is the scaffold written by `rabbot init`.
const defaultConfigYAML = `# config.yaml — Rabbot-SEO
data_dir: ""

control:
  port: 7777

log:
  level: info
  file: ""

crawler:
  user_agent: ""
  contact_email: ""   # MANDATORY — a valid email shown in the crawler User-Agent. Left empty on purpose: config-validate fails loudly until you fill it, so a crawl never goes out unreachable.
  egress_check_enabled: true
  egress_check_endpoint: "https://api.ipify.org"

defaults:
  min_interval: 10m
  max_interval: 24h
  per_host_concurrency: 2
  per_host_rate: 2s
  speed_scale: 100

sites: []

notifiers: []

routes: []

alerting:
  dedup_window: 5m
  per_recipient_hourly_cap: 30
  incident_auto_close_after: 24h
  digest:
    schedule: 1h
    severities: [info, warning]
`

func newInitCmd(bi BuildInfo) *cobra.Command {
	var (
		force            bool
		contactEmail     string
		siteURLs         []string
		siteName         string
		minInterval      string
		maxInterval      string
		speed            int
		maxPages         int
		authorized       bool
		connectClaude    string
		connectRemote    string
		connectRemoteBin string
		slackWebhook     string
		startDaemon      bool
		installService   bool
		withGrafana      bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up Rabbot-SEO (headless) or scaffold a config file",
		Long: "Three modes:\n\n" +
			"  Setup flags (--contact-email, --site, --i-am-authorized, …): non-interactive\n" +
			"  headless setup — writes a valid config.yaml and prints a summary.\n\n" +
			"  Bare invocation on a TTY (no flags): launches the interactive guided\n" +
			"  setup wizard. On an existing config it offers Add a site / Reconfigure /\n" +
			"  Cancel; re-running never corrupts the config.\n\n" +
			"  No flags and no TTY: scaffolds a commented config template (or errors if\n" +
			"  one already exists, unless --force is given).\n\n" +
			"  The crawler announces itself to every site via a published User-Agent that\n" +
			"  carries your --contact-email and a per-site trust signal: a site you have\n" +
			"  proven control of (rabbot verify) reads as \"verified for <site>\"; an\n" +
			"  unverified site whose domain matches your email reads as \"<site> contact,\n" +
			"  unverified\"; anything else reads as \"unverified — confirm or block\".",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgDir, err := config.ResolveConfigDir()
			if err != nil {
				return err
			}
			cfgPath := config.ConfigFilePath(cfgDir)

			// Headless setup path: triggered when ANY setup-related flag was
			// changed. Gating on the changed-state (not just contact-email/site
			// being non-empty) means partial input — e.g. `init --speed 50` or
			// `init --i-am-authorized` — still routes here and surfaces the
			// specific validation error, instead of silently falling through to
			// the scaffold branch and discarding the user's intent.
			setupFlags := []string{"contact-email", "site", "name", "min-interval", "max-interval", "speed", "max-pages", "i-am-authorized", "slack-webhook", "start", "install-service", "add-site", "with-grafana"}
			anySetupFlag := false
			for _, name := range setupFlags {
				if cmd.Flags().Changed(name) {
					anySetupFlag = true
					break
				}
			}
			if anySetupFlag {
				return runHeadlessSetup(cmd, bi, cfgPath, headlessInputs{
					contactEmail: contactEmail, siteURLs: siteURLs, siteName: siteName,
					minInterval: minInterval, maxInterval: maxInterval, speed: speed,
					maxPages:   maxPages,
					authorized: authorized, connectClaude: connectClaude,
					connectRemote: connectRemote, connectRemoteBin: connectRemoteBin,
					slackWebhook:   slackWebhook,
					startDaemon:    startDaemon,
					installService: installService,
					withGrafana:    withGrafana,
				})
			}

			// Connect-only path (#87): an explicit --connect-claude / --connect-remote
			// (and friends) with NO setup flag is a standalone "emit the MCP client
			// config" request, not a setup run. Before the fix these flags were absent
			// from setupFlags, so this case fell through to the scaffold branch below —
			// `init --connect-claude print --connect-remote you@host` wrote config.yaml
			// (and "wrote ...") and never printed the snippet. Route it to
			// emitConnectClaude instead: `print` prints the snippet ONLY (writes
			// nothing); a writable target (project|claude-code|claude-desktop)
			// merge-writes the MCP HOST config — neither path scaffolds the rabbot
			// config. It runs regardless of TTY because the flag is an explicit intent.
			if cmd.Flags().Changed("connect-claude") ||
				cmd.Flags().Changed("connect-remote") ||
				cmd.Flags().Changed("connect-remote-bin") {
				// Bake a non-default local data_dir into the printed/written snippet, as
				// the headless and wizard paths do (advisory; load error → "" → the
				// byte-identical legacy snippet).
				loaded, _ := config.Load(cfgPath, nil)
				emitConnectClaude(cmd, connectOpts{
					target:     connectClaude,
					remoteDest: connectRemote,
					remoteBin:  connectRemoteBin,
					dataDir:    loaded.DataDir,
				})
				return nil
			}

			// No setup flags but an interactive terminal: drive the TUI wizard.
			// Flags win over the TTY (handled above), so this only runs when the
			// user invoked a bare `rabbot init` at a real terminal.
			if isTTY() {
				// Existing-config sub-flow (spec step 1): if a config already exists,
				// offer Add a site / Reconfigure / Cancel BEFORE launching the fresh
				// wizard. Cancel is a quiet exit; Reconfigure runs the wizard.
				if _, statErr := os.Stat(cfgPath); statErr == nil {
					action, aerr := existingActionFn(cmd)
					if aerr != nil {
						return aerr
					}
					switch action {
					case wizard.ActionCancel:
						_, _ = fmt.Fprintln(cmd.OutOrStdout(), "setup cancelled.")
						return nil
					case wizard.ActionAddSite:
						// Adding a site interactively reuses the full wizard (which
						// collects + dedups via setup.Apply); the operator can also do
						// this headlessly with `init --add-site --site ...`.
						err := launchWizardFn(cmd, bi, cfgPath)
						if errors.Is(err, wizard.ErrCancelled) {
							return nil
						}
						return err
					case wizard.ActionReconfigure:
						// fallthrough to the wizard below
					}
				}
				err := launchWizardFn(cmd, bi, cfgPath)
				// An operator abort (Ctrl-C / Esc) is a normal action, not a
				// failure: exit quietly with no error banner and no usage dump.
				// The wizard already printed "setup cancelled." to stdout.
				if errors.Is(err, wizard.ErrCancelled) {
					return nil
				}
				return err
			}

			// No setup flags and no TTY: preserve the original scaffold behavior.
			if _, statErr := os.Stat(cfgPath); statErr == nil && !force {
				return fmt.Errorf("config already exists at %s (use --force to overwrite, or pass --contact-email/--site to run setup)", cfgPath)
			}
			if err := os.WriteFile(cfgPath, []byte(defaultConfigYAML), 0o600); err != nil {
				return err
			}
			if _, err := config.ResolveDataDir(""); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", cfgPath)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config file (scaffold mode)")
	// --add-site is inspected only via cmd.Flags().Changed("add-site") (it is one of
	// setupFlags, so passing it routes init to the headless append path that skips
	// the scaffold 'config already exists' guard). It has no value to read beyond
	// "was it set", so it is registered without a bound variable — binding one would
	// be dead. Use it WITH --site (and the setup flags); --add-site alone has no
	// sites to append and fails plan validation.
	cmd.Flags().Bool("add-site", false, "append the given --site(s) to an existing config (use with --site; skips the 'config already exists' guard; dedups)")
	cmd.Flags().StringVar(&contactEmail, "contact-email", "", "operator contact email shown in the crawler User-Agent (enables setup)")
	cmd.Flags().StringArrayVar(&siteURLs, "site", nil, "site URL to monitor (repeatable; enables setup)")
	cmd.Flags().StringVar(&siteName, "name", "", "human-readable name applied to the site(s)")
	cmd.Flags().StringVar(&minInterval, "min-interval", "", "minimum recheck interval override, e.g. 10m")
	cmd.Flags().StringVar(&maxInterval, "max-interval", "", "maximum recheck interval override, e.g. 24h")
	cmd.Flags().IntVar(&speed, "speed", 0, "speed-scale percent override, e.g. 100")
	cmd.Flags().IntVar(&maxPages, "max-pages", -1,
		"per-site page cap for the added site(s): 0 = monitor everything (unlimited), N = cap at N; unset leaves the default (2000)")
	cmd.Flags().BoolVar(&authorized, "i-am-authorized", false, "attest you are authorized to monitor the sites you add (required for setup)")
	cmd.Flags().StringVar(&connectClaude, "connect-claude", "print",
		"generate an MCP client config to connect Claude: print|project|claude-code|claude-desktop (advisory)")
	cmd.Flags().StringVar(&connectRemote, "connect-remote", "",
		"emit an SSH-transport connect snippet for a remote daemon (e.g. you@vps); the token stays on the remote host")
	cmd.Flags().StringVar(&connectRemoteBin, "connect-remote-bin", "",
		"remote rabbot binary name on the VPS PATH (default rabbot); only with --connect-remote")
	cmd.Flags().StringVar(&slackWebhook, "slack-webhook", "",
		"Slack Incoming-Webhook URL for alerts; env-interpolated (e.g. ${RABBOT_SLACK_WEBHOOK}) and never logged")
	cmd.Flags().BoolVar(&startDaemon, "start", false, "start the daemon now after setup (advisory; failure does not roll back config)")
	cmd.Flags().BoolVar(&installService, "install-service", false,
		"install the rabbot OS service so it runs 24/7 (may require elevated privileges; never auto-escalates)")
	cmd.Flags().BoolVar(&withGrafana, "with-grafana", false,
		"enable metrics and write the provisioned Prometheus + Grafana bundle (the non-TTY technical path; runs `observability init`; never runs docker)")
	return cmd
}

type headlessInputs struct {
	contactEmail     string
	siteURLs         []string
	siteName         string
	minInterval      string
	maxInterval      string
	speed            int
	maxPages         int
	authorized       bool
	connectClaude    string
	connectRemote    string
	connectRemoteBin string
	slackWebhook     string
	startDaemon      bool
	installService   bool
	withGrafana      bool
}

func runHeadlessSetup(cmd *cobra.Command, bi BuildInfo, cfgPath string, in headlessInputs) error {
	sites := make([]setup.SiteInput, 0, len(in.siteURLs))
	for _, u := range in.siteURLs {
		sites = append(sites, setup.SiteInput{
			URL:         u,
			Name:        in.siteName,
			MinInterval: in.minInterval,
			MaxInterval: in.maxInterval,
			Speed:       in.speed,
		})
	}
	plan := setup.Plan{ContactEmail: in.contactEmail, Authorized: in.authorized, Sites: sites}

	// Validate before touching disk so a partial/invalid invocation does not
	// leave an empty scaffold behind that misrepresents what the user asked for.
	if err := plan.Validate(); err != nil {
		return err
	}

	// --max-pages is optional; -1 is the "unset / leave default" sentinel. When set
	// it must be >= 0 (0 = unlimited, N = cap at N). Reject a negative cap up front so
	// a typo does not silently fall through to the default.
	if cmd.Flags().Changed("max-pages") && in.maxPages < 0 {
		return fmt.Errorf("setup: --max-pages must be >= 0 (0 = unlimited)")
	}

	// Start from the commented scaffold when there is no file yet, so the
	// comment-preserving writers produce a readable, fully-structured config.
	if _, statErr := os.Stat(cfgPath); os.IsNotExist(statErr) {
		if err := os.WriteFile(cfgPath, []byte(defaultConfigYAML), 0o600); err != nil {
			return err
		}
	}
	if _, err := config.ResolveDataDir(""); err != nil {
		return err
	}

	res, err := plan.Apply(setup.Options{ConfigPath: cfgPath, Version: bi.Version, Now: time.Now()})
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "configured %s\n", res.ConfigPath)
	_, _ = fmt.Fprintf(out, "user-agent: %s\n", res.UserAgent)
	for _, s := range res.SitesAdded {
		_, _ = fmt.Fprintf(out, "added site: %s\n", s)
	}
	for _, s := range res.SitesSkipped {
		_, _ = fmt.Fprintf(out, "site already present, skipped: %s\n", s)
	}

	// Spec B headless: if --max-pages was set, write the per-site cap for each freshly
	// added site BEFORE reloading, so the coverage line below reflects the chosen cap.
	// 0 = unlimited; N = cap at N. AddSiteYAML never writes a discovery block, so the cap
	// MUST be a separate per-site write via SetSiteMaxPagesYAML (keyed by URL —
	// SetKeyYAML cannot index the sites sequence).
	if cmd.Flags().Changed("max-pages") && in.maxPages >= 0 {
		for _, u := range res.SitesAdded {
			if werr := config.SetSiteMaxPagesYAML(cfgPath, u, in.maxPages); werr != nil {
				return werr
			}
		}
		// A requested --site that deduped to already-present lands in SitesSkipped, not
		// SitesAdded, so the cap above never reaches it — the cap intent would be
		// silently dropped. Surface a one-line stderr notice naming the skipped site(s)
		// so the operator knows the cap was not applied (and can set it explicitly with
		// `rabbot config set` if intended). Advisory: it never fails setup.
		if len(res.SitesSkipped) > 0 {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"note: --max-pages not applied to already-present site(s): %s\n",
				strings.Join(res.SitesSkipped, ", "))
		}
	}

	// Phase 4 (Spec A): a one-line coverage estimate per freshly added site
	// (best-effort sitemap auto-count). Production keeps the SSRF guard on
	// (allowPrivate=false); it never fails setup. We reload the written config so the
	// estimate reflects the just-applied per-site budget (including any --max-pages cap
	// written above) and the D8 capped phrasing.
	if loadedCfg, lerr := config.Load(cfgPath, nil); lerr == nil {
		emitCoverageForAddedSites(cmd.Context(), out, &loadedCfg, res.SitesAdded, bi.Version, false)
	}

	// Step 8 — alerts (optional): write the Slack notifier and send a best-effort
	// test alert. A config-write failure aborts (spec error-handling); the send is
	// advisory. NEVER prints the webhook URL.
	if err := applyAlertsStep(cmd, cfgPath, in.slackWebhook); err != nil {
		return err
	}

	// Connect-Claude is advisory and non-blocking: a failure here never fails
	// setup (mirroring how alerts are advisory). The snippet always goes to stderr
	// so a headless user gets a copy even when no file is written. dataDir/config
	// stay empty in the common case, so SnippetWithDirs bakes nothing (byte-
	// identical to the legacy snippet); --connect-remote opts into the SSH variant.
	//
	// Phase 6 Task 6: bake the daemon's NON-DEFAULT data_dir into the local snippet.
	// NOTE: this baking does NOT influence Hop-2 reachability today. The control
	// token lives in the CONFIG dir (run.go writes it under config.ResolveConfigDir),
	// not data_dir; the mcp child reads it from the config dir keyed off --config
	// (helpers.go newControlClientFromConfigDir), and the child no longer opens the
	// DB at all (Phase 1: NewControlBridge reads everything over the control client).
	// So --data-dir affects neither the token nor the control port. The only existing
	// customization paths are env vars (XDG_*, RABBOT_DATA_DIR), which a
	// Claude-spawned child inherits regardless — so coherence is already handled by
	// environment inheritance. The data_dir baking is retained for forward-compat
	// with a future daemon dir flag; config-dir baking is deferred (plan D2/D8 scope
	// pin). We reload the effective config (env > file) to read DataDir; the load
	// error is non-fatal (connect is advisory) and leaves DataDir "", giving the
	// byte-identical legacy snippet. A default data_dir is "" so SnippetWithDirs/
	// serverEntry bake nothing — only a custom dir activates it.
	loaded, _ := config.Load(cfgPath, nil)
	emitConnectClaude(cmd, connectOpts{
		target:     in.connectClaude,
		remoteDest: in.connectRemote,
		remoteBin:  in.connectRemoteBin,
		dataDir:    loaded.DataDir,
	})

	// --with-grafana — the non-TTY technical path: run the SAME generator the
	// wizard's technical path and `observability init` run (writes the bundle +
	// sets metrics.addr + prints next steps, identical bytes), prompting nothing.
	// Advisory: a generator failure surfaces but never rolls back the written
	// config (mirroring the other optional steps). The config was written above,
	// so the comment-preserving SetKeyYAML has a well-formed document to mutate.
	if in.withGrafana {
		if err := runWithGrafana(cmd); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "could not enable observability (config is written): %v\n", err)
		}
	}

	// Step 10 — run it (offer): install a system service and/or start the daemon.
	// Both are advisory — a failure surfaces the OS error but never rolls back the
	// already-written config (spec error-handling).
	applyRunServiceStep(cmd, bi, in.installService, in.startDaemon)

	// Step 11 — summary: render the shared, UI-free summary from the just-written
	// config so the headless path and the wizard runner share one renderer. The
	// webhook is NEVER passed in — SlackConfigured is read from the resulting
	// config state (same as the wizard path), so a re-run that omits
	// --slack-webhook still reports "configured" when Slack is already in the file.
	renderSetupSummary(cmd, cfgPath, slackConfiguredFromConfig(cfgPath))
	return nil
}

// connectReminderText is the fixed Connect-Claude reminder shown in the summary
// (step 11). It carries no token and is safe to print. The reminder points to
// the `rabbot mcp print` command (always available, no setup re-run needed)
// rather than `rabbot init --connect-claude`, which is a setup flag that
// requires the full --contact-email/--site/--i-am-authorized set and errors on a
// config that already exists when used without them.
const connectReminderText = "Connect Claude later: run `rabbot mcp print` to get the MCP snippet, then add it to your MCP host config."

// renderSetupSummary reloads the written config, resolves the data dir, and writes
// the shared setup.RenderSummary block to the command's stdout. It is reused by
// BOTH the headless path and the wizard runner (spec §H: one UI-free renderer). A
// reload/resolve failure is advisory — the config is already written, so a summary
// hiccup must not fail setup; it degrades to a short notice. The webhook is never
// passed in: SummaryFromConfig only takes the SlackConfigured bool.
func renderSetupSummary(cmd *cobra.Command, cfgPath string, slackConfigured bool) {
	out := cmd.OutOrStdout()
	loaded, err := config.Load(cfgPath, nil)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "could not render summary (config is written): %v\n", err)
		return
	}
	dataDir, err := config.ResolveDataDir(loaded.DataDir)
	if err != nil {
		dataDir = loaded.DataDir
	}
	summary := setup.SummaryFromConfig(loaded, cfgPath, dataDir, slackConfigured, connectReminderText)
	if err := setup.RenderSummary(out, summary); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "could not render summary (config is written): %v\n", err)
	}
}

// applyRunServiceStep implements onboarding step 10 (run it) for BOTH the
// headless path and the wizard runner. install-service is offered first: it ALWAYS
// prints an elevation notice BEFORE calling the install seam (spec §H security:
// state when elevation is needed, NEVER silently escalate — rabbot never
// auto-sudos; the OS prompts/refuses if unprivileged). Then, if requested, it
// starts the daemon. Both steps are advisory: a failure surfaces the OS error but
// does NOT roll back the written config (spec error-handling).
func applyRunServiceStep(cmd *cobra.Command, bi BuildInfo, install, start bool) {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	if install {
		_, _ = fmt.Fprintln(out, "Installing a system service may require elevated privileges "+
			"(sudo/Administrator); you will be prompted by the OS — rabbot never escalates silently.")
		if err := installServiceFn(cmd, bi); err != nil {
			_, _ = fmt.Fprintf(errOut, "service install failed (you can retry with `rabbot service install`): %v\n", err)
		} else {
			_, _ = fmt.Fprintln(out, "system service installed.")
		}
	}
	if start {
		if err := startDaemonFn(cmd, bi); err != nil {
			_, _ = fmt.Fprintf(errOut, "could not start the daemon (you can run `rabbot run` yourself): %v\n", err)
		}
	}
}

// installServiceNow installs the rabbot OS service, REUSING the kardianos
// wiring from newService — it mirrors `rabbot service install` exactly and does
// NOT reimplement the service definition. It never auto-elevates: kardianos
// surfaces the OS permission error if the process is unprivileged, and the caller
// (applyRunServiceStep) has already printed the elevation notice.
func installServiceNow(cmd *cobra.Command, bi BuildInfo) error {
	svc, _, err := newService()
	if err != nil {
		return err
	}
	return svc.Install()
}

// startDaemonInProcess starts the daemon for the run-now step by exec-ing the
// resolved `rabbot run` as a detached child process, rather than blocking the
// init process inside runDaemon. This is the lower-risk choice: it reuses the
// exact run command (which itself resolves config/token/data dir), keeps init
// returning promptly to print the summary, and lets the new process own the
// daemon lifecycle. A start failure is advisory (the caller surfaces it without
// rolling back config).
func startDaemonInProcess(cmd *cobra.Command, _ BuildInfo) error {
	bin := mcpsrv.ResolveBinary()
	// G204: the command is NOT user-controlled — bin is our own resolved
	// executable path (os.Executable, falling back to "rabbot" on PATH) and the
	// sole argument is the fixed literal "run". No operator input reaches argv.
	run := exec.Command(bin, "run") // #nosec G204 -- self-exec of `rabbot run`; no user input in argv
	// Detach: the daemon must outlive init. We do not wire its stdio to init's
	// (the operator runs `rabbot status` to observe it).
	if err := run.Start(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "started the daemon (pid %d); check it with `rabbot status`, stop it with `rabbot stop`.\n", run.Process.Pid)
	// Release the child so it is not reaped/awaited by init.
	_ = run.Process.Release()
	return nil
}

// slackNotifierName is the fixed name the onboarding alerts step gives the Slack
// notifier (and the route that points at it). Keeping it a single constant ties
// the notifier and its fallback route together and makes the re-run dedup (by
// name / by notifier) deterministic.
const slackNotifierName = "slack"

// applyAlertsStep implements onboarding step 8 (alerts) for BOTH the headless
// path and the wizard runner. When webhook != "", it writes a
// {slack, slack-webhook, <url>} notifier AND a default fallback route pointing at
// it to config (a write failure aborts setup per spec error-handling), then sends
// a best-effort test alert via the sendTestAlertFn seam — a send failure is
// advisory (a warning, never fatal) because the daemon is not up yet to retry.
//
// TYPE CONTRACT: the notifier type MUST be "slack-webhook" — that is the only
// type supervisor.BuildAlertingStack accepts (it returns a hard error for any
// other value, so a config with type "slack" fails daemon startup). The canonical
// type is asserted end-to-end by feeding the written config through
// supervisor.BuildAlertingStack in the CLI tests.
//
// ROUTE CONTRACT: a notifier with no route is unreachable — the dispatcher
// iterates cfg.Routes and there is no implicit fallback to all notifiers when the
// list is empty, so without a route a configured Slack would never receive a real
// change alert. We write a fallback route (empty Match) targeting the notifier.
// Both writes are idempotent (dedup by name / by notifier), so a re-run never
// duplicates either entry.
//
// SECRET-SAFETY: the webhook value flows verbatim into AddNotifierYAML (so an
// ${ENV} token survives to disk) and is NEVER printed. The success line carries
// no URL; the underlying notifier already scrubs the webhook from any error it
// returns, so the advisory warning is leak-safe.
func applyAlertsStep(cmd *cobra.Command, cfgPath, webhook string) error {
	if webhook == "" {
		return nil
	}
	if err := config.AddNotifierYAML(cfgPath, config.NotifierConfig{
		Name: slackNotifierName, Type: "slack-webhook", URL: webhook,
	}); err != nil {
		return fmt.Errorf("write slack notifier: %w", err)
	}
	// Wire the notifier into the alert pipeline with a default fallback route, or
	// the notifier is never reached for real change alerts.
	if err := config.AddRouteYAML(cfgPath, config.RouteConfig{
		Notifier: slackNotifierName,
	}); err != nil {
		return fmt.Errorf("write slack route: %w", err)
	}
	// cobra's Execute guarantees a non-nil context in RunE, so this guard is dead in
	// production; it exists so direct unit-test calls (no Execute) get a usable context.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	// Bound the advisory test-alert: cmd.Execute() runs with no context, so the
	// ambient ctx is effectively context.Background() (no deadline). A pathological
	// endpoint returning repeated 429s would otherwise drive the slack retry loop
	// (up to maxSlackRetries+1 attempts, each waiting up to maxRetryAfter≈60s plus
	// the 30s request timeout — see internal/notify/slack.go) and stall onboarding
	// for minutes. The slack backoff wait already selects on ctx.Done, so this
	// deadline aborts any in-flight retry promptly. Spec: the test-alert is
	// best-effort and MUST NOT block setup.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// #84: the daemon interpolates ${ENV} webhook tokens at config Load (os.Expand
	// with os.Getenv); this immediate, daemon-less test alert must do the same or it
	// would POST the LITERAL "${RABBOT_SLACK_WEBHOOK}" string and fail with an
	// "unsupported protocol scheme" the operator can't act on. os.ExpandEnv IS that
	// same expansion (os.Expand over os.Getenv), so the test exercises the URL the
	// daemon will actually use. When the token does NOT resolve (the env var is unset
	// at init time — common: the secret is supplied to the service at runtime), there
	// is nothing valid to POST, so SKIP the test with a clear note rather than POST a
	// literal token. The literal ${ENV} form is still written to config verbatim
	// (above), so the daemon resolves it at runtime.
	testURL := os.ExpandEnv(webhook)
	if strings.Contains(webhook, "${") && (testURL == webhook || strings.Contains(testURL, "${") || testURL == "") {
		// Unresolved env token: skip the live test. SECRET-SAFETY: only the
		// UN-resolved token form (e.g. "${RABBOT_SLACK_WEBHOOK}") is echoed — never a
		// resolved secret.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"note: Slack webhook %s is supplied at runtime (env var unset now); skipping the immediate test alert. "+
				"After the daemon has the secret, verify with `rabbot notify test %s`.\n",
			webhook, slackNotifierName)
	} else if err := sendTestAlertFn(ctx, testURL, nil); err != nil && !errors.Is(err, notify.ErrNoWebhook) {
		// The error is already webhook-scrubbed by the slackNotifier; safe to print.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "test alert failed (you can re-test later with `rabbot notify test %s`): %v\n", slackNotifierName, err)
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Slack alerts configured.")
	return nil
}

// promptExistingAction presents the Add a site / Reconfigure / Cancel choice when
// `rabbot init` finds an existing config on a TTY (spec step 1). It collects a
// choice via a huh.Select and maps it to a wizard.ExistingAction via the pure
// wizard.ResolveExistingAction. A user abort (Ctrl-C/Esc) maps to ActionCancel so
// it reads as a quiet exit rather than an error.
//
// UNTESTED SEAM: the huh.Form.Run call requires a real terminal, so it is
// exercised only by an integration run; the ROUTING (existingActionFn) and the
// pure mapping (wizard.ResolveExistingAction) are unit-tested.
func promptExistingAction(cmd *cobra.Command) (wizard.ExistingAction, error) {
	choice := "add"
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("A config already exists — what would you like to do?").
				Options(
					huh.NewOption("Add a site to the existing config", "add"),
					huh.NewOption("Reconfigure (re-run the full setup)", "reconfigure"),
					huh.NewOption("Cancel", "cancel"),
				).
				Value(&choice),
		),
	).WithInput(cmd.InOrStdin()).WithOutput(cmd.OutOrStdout())
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return wizard.ActionCancel, nil
		}
		return 0, err
	}
	return wizard.ResolveExistingAction(choice)
}

// offerSeeItWork asks, right after go-live, whether to show the "here's what we
// just read from your site" summary. It mirrors promptExistingAction's huh seam:
// a yes returns true; a no, or an operator abort (Ctrl-C/Esc), returns false so
// declining is a quiet skip, not an error. The caller only renders the summary on
// true, so a false here simply falls through to the optional upgrade menu.
//
// UNTESTED SEAM: the huh.Form.Run call requires a real terminal, so it is
// exercised only by an integration run; the ROUTING into it (offerSeeItWorkFn) is
// what the init tests exercise.
func offerSeeItWork(cmd *cobra.Command) bool {
	var show bool
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Want to see it work now?").
				Description("We'll show you what we just read from your homepage — no waiting.").
				Affirmative("Yes, show me").
				Negative("Skip").
				Value(&show),
		),
	).WithInput(cmd.InOrStdin()).WithOutput(cmd.OutOrStdout())
	if err := form.Run(); err != nil {
		// A user abort (Ctrl-C/Esc) is a quiet skip, not a failure — the user is
		// already live and this is entirely optional.
		return false
	}
	return show
}

// connectOpts carries the connect-claude routing: the write target, an optional
// SSH remote destination (+ remote bin), and the resolved local data/config dir
// to bake when non-default.
type connectOpts struct {
	target     string // print|project|claude-code|claude-desktop
	remoteDest string // "you@vps" → emit the SSH snippet instead of a local one
	remoteBin  string // remote binary name (default rabbot) — only with remoteDest
	dataDir    string // resolved local data dir; baked only when non-default
	configPath string // resolved local config path; baked only when non-default
}

// emitConnectClaude handles the headless Connect-Claude step: it prints the
// copyable snippet to stderr, and for a writable target merge-writes the config.
// Remote mode emits the SSH-transport snippet; local mode bakes any non-default
// dirs. Advisory: any error is reported to stderr but never fails setup.
func emitConnectClaude(cmd *cobra.Command, o connectOpts) {
	errOut := cmd.ErrOrStderr()
	_, _ = fmt.Fprintln(errOut, "Connect Claude (MCP) — add this to your MCP host config:")
	if o.remoteDest != "" {
		_, _ = fmt.Fprintln(errOut, mcpsrv.RemoteSnippet(o.remoteDest, o.remoteBin))
	} else {
		_, _ = fmt.Fprintln(errOut, mcpsrv.SnippetWithDirs(mcpsrv.ResolveBinary(), o.dataDir, o.configPath))
	}

	if o.target == "" || o.target == "print" {
		return
	}
	tgt, err := mcpsrv.ParseTarget(o.target)
	if err != nil {
		reportConnectWrite(errOut, o.target, "", err)
		return
	}
	if tgt == mcpsrv.TargetPrint {
		return
	}
	path, perr := mcpsrv.TargetPath(tgt)
	if perr != nil {
		reportConnectWrite(errOut, o.target, "", perr)
		return
	}
	var werr error
	switch {
	case o.remoteDest != "":
		werr = connectWriteRemote(path, o.remoteDest, o.remoteBin)
	case o.dataDir != "" || o.configPath != "":
		werr = connectWriteDirs(path, mcpsrv.ResolveBinary(), o.dataDir, o.configPath)
	default:
		path, werr = connectWriteFn(o.target) // default path keeps the existing seam (tests rely on it)
	}
	reportConnectWrite(errOut, o.target, path, werr)
}

// reportConnectWrite surfaces the outcome of a Connect-Claude write to out so both
// the headless and wizard paths give identical, advisory feedback: the written
// path on success, or a non-fatal "could not write" line on failure. The config
// carries no secret (no token/env), so printing the path is safe. It is the single
// place the two paths funnel their connect-write feedback, so they never drift.
func reportConnectWrite(out io.Writer, target, path string, err error) {
	if err != nil {
		_, _ = fmt.Fprintf(out, "connect-claude: could not write %s config: %v\n", target, err)
		return
	}
	if path != "" {
		_, _ = fmt.Fprintf(out, "connect-claude: wrote %s\n", path)
	}
}

// loadWizardConfig resolves the config the wizard should start from: the
// user's config.yaml when one exists on disk (so a re-run pre-fills the scope
// form with the configured cadence/scope and the precheck uses the configured
// access/UA), falling back to the factory defaults on a first run with no file.
// Extracted as a pure, testable seam so the "prefill from loaded config, not
// factory" contract is unit-pinned without driving the TTY wizard.
func loadWizardConfig(cfgPath string) config.Config {
	cfg := config.Defaults()
	if loaded, err := config.Load(cfgPath, nil); err == nil {
		cfg = loaded
	}
	return cfg
}

// firstSiteURL returns the URL of the first site the wizard collected, or "" when
// the collection is empty. The essential path always populates exactly one site,
// but the empty guard keeps the go-live wiring panic-free against a malformed
// collection (an empty URL degrades the go-live host to "", never a crash).
func firstSiteURL(in wizard.Inputs) string {
	if len(in.Sites) == 0 {
		return ""
	}
	return in.Sites[0].URL
}

// fullSpeedHint builds the plain-language closing nudge shown after the summary
// for the first site still UNVERIFIED — the common case, since the verify upgrade
// is optional in the new flow. It restates that one upgrade with a concrete payoff
// ("go full speed") and the exact command to run, so an operator who skipped the
// menu still knows the single next step that lifts the throttle. The verified
// state is read from the just-written CONFIG (VerifiedAt set), NOT the stale
// wizard.Inputs: a menu verify writes the config block but never mutates the
// in-memory draft, so reading config is the honest source — an operator who DID
// verify in the menu is not nagged. A fully-verified config (or none) returns ""
// (no hint, no nag). A config-load failure degrades to "" — the config is already
// written and live, so a missing nudge must never break setup. It is a pure
// mapping over the config path, so it is unit-tested without a TTY.
func fullSpeedHint(cfgPath string) string {
	loaded, err := config.Load(cfgPath, nil)
	if err != nil {
		return ""
	}
	for _, s := range loaded.Sites {
		if s.URL == "" || s.Verification.VerifiedAt != "" {
			continue
		}
		return "Tip: run `rabbot verify " + s.URL + "` to prove you control it and go full speed."
	}
	return ""
}

// launchWizard runs the interactive TUI onboarding flow and persists its result.
// It wires the real precheck/verify/token seams into wizard.Deps, runs the
// wizard, applies the resulting setup.Plan via the shared setup core, and then —
// for any site that carries a placed proof token (verified OR throttled) —
// records the proof-of-control INTENT in config.yaml
// (config.SetSiteVerificationYAML), NOT the DB: at `init` time the daemon doesn't
// exist and the site isn't in the store yet, so the daemon re-verifies
// authoritatively later (spec §E "living state").
//
// UNTESTED SEAM: this function drives a real terminal (huh + bubbletea), so it
// is covered only by an integration run, never by unit tests. The ROUTING into
// it (isTTY + launchWizardFn) is what the init tests exercise.
func launchWizard(cmd *cobra.Command, bi BuildInfo, cfgPath string) error {
	// cobra's Execute guarantees a non-nil context in RunE, so this guard is dead in
	// production; it exists so direct unit-test calls (no Execute) get a usable context.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// Resolve UA/access AND the scope-form prefill from whatever config exists
	// today: the wizard collects the real contact URL, but the one-shot precheck
	// only needs a self-identifying UA, and a re-run must pre-fill the scope step
	// with the user's CONFIGURED cadence/scope (not factory) so clicking through
	// preserves it rather than overwriting it.
	cfg := loadWizardConfig(cfgPath)

	key, err := ensureWizardInstanceKey(cfg)
	if err != nil {
		return fmt.Errorf("load instance key: %w", err)
	}

	deps := wizard.Deps{
		In:      cmd.InOrStdin(),
		Out:     cmd.OutOrStdout(),
		Version: bi.Version,
		// Pre-fill the scope/cadence form from the loaded config, not the factory
		// defaults — on first run (no file) loadWizardConfig returns the factory
		// values anyway, so this is correct for both paths.
		Defaults: cfg,
		Precheck: func(c context.Context, url string) (precheck.Report, error) {
			access := cfg.AccessForBaseURL(url)
			req := fetcher.Request{
				URL:       url,
				Headers:   access.Headers,
				BasicUser: access.BasicUser,
				BasicPass: access.BasicPass,
				ProxyURL:  access.ProxyURL,
			}
			return precheck.Run(c, url, precheck.Options{
				// Single-target diagnostic for the one typed host (verified=false — the
				// wizard runs pre-verification); the per-site trust signal is accurate
				// for a diagnostic. The crawler.user_agent override still wins inside.
				UserAgent:    cfg.UserAgentFor(hostFromURL(url), bi.Version, false),
				Request:      req,
				AllowPrivate: false,
			})
		},
		Verify: func(c context.Context, host string, method verify.Method) (verify.Outcome, error) {
			return verify.Verify(c, verify.Request{
				Host:   host,
				Method: method,
			}, verify.Options{Now: time.Now().UTC(), Key: key})
		},
		Derive: func(host string) string { return verify.DeriveToken(key, host) },
		// CountPages backgrounds the bounded, SSRF-guarded (allowPrivate=false) sitemap
		// count behind the wizard's injectable seam; the wizard ctx-cancels it when the
		// form ends, so it never outlives the form or blocks init exit (Spec B Phase 2/4).
		CountPages: productionCountPages(&cfg, bi.Version),
		Now:        func() time.Time { return time.Now().UTC() },
	}

	in, err := wizard.Run(ctx, deps)
	if err != nil {
		return err
	}

	res, err := persistWizardResult(cmd, in, cfgPath, bi, cmd.ErrOrStderr())
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "configured %s\n", res.ConfigPath)
	_, _ = fmt.Fprintf(out, "user-agent: %s\n", res.UserAgent)
	for _, s := range res.SitesAdded {
		_, _ = fmt.Fprintf(out, "added site: %s\n", s)
	}

	// Go-live: this is the payoff. Run the precheck for the first site purely to
	// render a plain, advisory verdict line (it never gates), then auto-start the
	// daemon detached so "You're live" is real — the operator's first site is being
	// watched the moment the essential path finishes, with NO technical step.
	// The precheck is best-effort: if it errors we still print the go-live line and
	// start the daemon; the operator just gets no caveat line. host is derived from
	// the FIRST collected site (firstSiteURL is empty-guarded).
	host := hostFromURL(firstSiteURL(in))
	_, _ = fmt.Fprintln(out, wizard.GoLiveLine(host))
	// B7 sleep-nudge: if this host looks like a laptop, print one honest advisory line
	// here — right after go-live and before the upgrade menu — so the menu's
	// "Keep watching 24/7" service row lands with context. Best-effort and wizard-only;
	// the headless path never reaches this (it returns from runHeadlessInit), so scripts
	// stay byte-stable (criterion 10).
	maybePrintSleepNudge(out)
	// Run the go-live precheck ONCE and keep the report: it feeds the plain verdict
	// line here AND the optional "see it work" summary below, so the newcomer-facing
	// payoff never costs a second fetch. perr != nil ⇒ rep is the zero Report, which
	// both consumers handle (no verdict caveat / no title bullet) without crashing.
	rep, perr := deps.Precheck(ctx, firstSiteURL(in))
	if perr == nil {
		_, _ = fmt.Fprintln(out, wizard.PlainVerdict(rep, host))
	}
	// Start the daemon detached via the SAME seam the headless --start path uses
	// (startDaemonFn). It is advisory: a start failure surfaces a fix-it line on
	// stderr but never rolls back the just-written config — "you're configured"
	// stays true even if "you're running" momentarily isn't.
	if serr := startDaemonFn(cmd, bi); serr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"could not start monitoring now (run `rabbot run` yourself): %v\n", serr)
	}

	// "See it work" (optional): immediately after go-live, offer to show what the
	// pipeline just read off the homepage, so a newcomer watches it do something real
	// before trusting it. It REUSES the go-live precheck report (no second fetch) and
	// is skippable; an abort or a declined confirm just falls through to the menu. The
	// summary is honest — it reports what we read now, never that a change happened.
	if perr == nil && offerSeeItWorkFn(cmd) {
		_, _ = fmt.Fprint(out, wizard.SeeItWorkSummary(rep))
	}

	// REQUIRED alerts step (decision #23): before the optional menu, the operator
	// makes an EXPLICIT alerts choice — configure Slack / email / a webhook, OR
	// deliberately acknowledge "no alerts — pull-only (CLI/MCP) mode". There is no
	// silent skip: the loop re-prompts on an abort because "no alerts" is a visible
	// option to pick. Skipped only when alerts are ALREADY configured (a re-run, or
	// the merged headless `--slack-webhook` path) so a configured operator is not
	// forced to re-choose. It is advisory to the wizard's success: a TTY hiccup logs
	// to stderr and falls through (the operator is already live).
	if !slackConfiguredFromConfig(cfgPath) {
		runAlertsStep(cmd, cfgPath)
	}

	// The go-live precheck (rep) carries the homepage HTML the verify step sniffs
	// for a platform hint; perr != nil leaves rep the zero Report, so rawHTML is
	// nil and the sniff falls back to PlatformUnknown (plain choices, no claim).
	// Computed BEFORE the menu because the per-action dispatch closure consumes it.
	var rawHTML []byte
	if perr == nil {
		rawHTML = rep.Doctor.RawHTML
	}
	// Post-go-live upgrade menu (Item D): a RE-ENTERABLE single-pick loop of the optional
	// follow-ons. The operator picks one action, it runs, the menu re-appears, and they pick
	// another — finishing only on the explicit "I'm all set" row. Esc at the menu is a clean
	// no-op back-out (the user is already live), NEVER a wizard crash (finding #3). The
	// per-upgrade ACTIONS live here in the runner (mirroring how persistWizardResult owns
	// side-effects), not in the wizard package, so the live huh form stays the only TTY
	// surface. A menu error is advisory — the user is already live, so we surface it to
	// stderr and fall through to the summary rather than failing setup.
	dispatchUpgrade := func(u wizard.Upgrade) {
		switch u {
		case wizard.UpgradeVerify:
			runVerifyUpgrade(cmd, bi, cfgPath, key, firstSiteURL(in), rawHTML)
		case wizard.UpgradeAlerts:
			runAlertsUpgrade(cmd, cfgPath)
		case wizard.UpgradeConnectGSC:
			// "Connect Google Search Console" — collect the property + 0600 credential
			// PATH (never a body), write the per-site gsc block, then offer the doctor
			// connectivity check. Skippable (lossless); a TTY abort is a quiet skip.
			runConnectGSCUpgrade(cmd, cfgPath, firstSiteURL(in))
		case wizard.UpgradeService:
			// Install the OS service so monitoring survives logout/reboot. Install
			// only (start=false) — the daemon was already auto-started at go-live.
			applyRunServiceStep(cmd, bi, true, false)
		case wizard.UpgradeConnectClaude:
			// EXISTING behavior, unchanged (spec D5): print the copyable MCP snippet.
			emitConnectClaude(cmd, connectOpts{target: "print"})
		case wizard.UpgradeGrafana:
			// "See it on a dashboard" — sizing copy, then the Claude-vs-technical fork
			// (decision 18). The Claude arm writes no bundle/no config; the technical
			// arm runs the shared generator inline.
			runGrafanaUpgrade(cmd, cfgPath)
		case wizard.UpgradeFineTune:
			runFineTuneUpgrade(cmd, cfgPath)
		}
	}
	if merr := wizard.PromptUpgradeMenu(cmd.InOrStdin(), out, dispatchUpgrade); merr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "skipping the optional menu (%v)\n", merr)
	}

	// Always surface the Connect-Claude snippet so the user has it regardless of
	// the write decision (the snippet carries no token; safe to print).
	_, _ = fmt.Fprintln(out, "Connect Claude (MCP) — add this to your MCP host config:")
	_, _ = fmt.Fprintln(out, mcpsrv.Snippet(mcpsrv.ResolveBinary()))

	// Summary: the SAME UI-free renderer the headless path uses, fed the
	// just-written config so the wizard and flags paths share one renderer
	// (spec §H). It runs AFTER the optional upgrade menu above, so its per-site
	// state words reflect any verify/alerts/fine-tune the operator just ran.
	// SlackConfigured is read from the just-written config's notifiers
	// (persistWizardResult writes them), so the wizard need not thread the secret
	// webhook into the summary call at all.
	renderSetupSummary(cmd, cfgPath, slackConfiguredFromConfig(cfgPath))

	// Closing nudge: if a site is still unverified in the just-written config (the
	// common case — the verify upgrade is optional), restate the single next step
	// that lifts the throttle in plain language. Reading config (not the stale in)
	// means an operator who verified through the menu above is not nagged.
	// fullSpeedHint is "" for a verified or empty config, so nothing prints when
	// there is nothing to nudge.
	if hint := fullSpeedHint(cfgPath); hint != "" {
		_, _ = fmt.Fprintln(out, hint)
	}
	return nil
}

// runVerifyUpgrade is the post-go-live "unlock full-speed monitoring" (verify)
// action dispatched from the upgrade menu. It drives the place-THEN-verify proof
// screen (spec §V): sniff the platform from the go-live homepage HTML to
// recommend the easiest method, derive the instance-bound token from key+host,
// run the live screen (which shows the token and waits for an explicit
// "check now" before checking — never on entry), and on a VERIFIED outcome
// record the proof-of-control intent.
//
// Persistence mirrors runVerify's two writes but is robust to init-time reality:
// the config verification block (the authoritative INTENT at init time, spec §E)
// is always written via writeVerificationIntent on success; the DB proof record
// is a BEST-EFFORT SaveVerification — the daemon was just auto-started and may
// not have inserted the site row yet, so a missing row / open failure is skipped
// silently (the daemon re-verifies the living state authoritatively). A cancel or
// a miss writes nothing — the site simply stays throttled.
//
// UNTESTED SEAM: the live screen (wizard.RunProofScreen → tea.Program.Run) needs
// a real terminal, so this is exercised only by an integration `rabbot init`;
// the pure helpers it composes (SniffPlatform, RecommendMethod, the proof model)
// are unit-tested. cfgPath/key/siteURL/rawHTML are the inputs it consumes.
func runVerifyUpgrade(cmd *cobra.Command, _ BuildInfo, cfgPath string, key []byte, siteURL string, rawHTML []byte) {
	out := cmd.OutOrStdout()
	host := hostFromURL(siteURL)

	// Sniff the platform from the go-live homepage. The proof screen consumes it to
	// recommend the easiest method (pre-highlighted on the V2 picker) and to pick
	// the right provider deep-link on the V3 placement screen; an unknown platform
	// degrades gracefully (sensible default, no claim, generic how-to).
	platform := precheck.SniffPlatform(string(rawHTML))

	// The token is DERIVED from the instance key bound to the host — never
	// caller-supplied — so it matches what `rabbot verify` would later derive.
	token := verify.DeriveToken(key, host)

	// cobra's Execute guarantees a non-nil context in RunE, so this guard is dead in
	// production; it exists so direct unit-test calls (no Execute) get a usable context.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// The verify closure runs the instance-bound check against the method the
	// operator PICKED on the screen; the screen dispatches it ONLY on an explicit
	// "check now"/"check again" (place-then-verify).
	vf := func(c context.Context, method verify.Method) (verify.Outcome, error) {
		return verify.Verify(c, verify.Request{Host: host, Method: method},
			verify.Options{Now: time.Now().UTC(), Key: key})
	}

	outcome, cancelled, err := wizard.RunProofScreen(ctx, cmd.InOrStdin(), out, platform, host, token, vf)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "verification screen ended early: %v\n", err)
		return
	}
	if cancelled || outcome.Record.State != verify.StateVerified {
		// Cancel or miss: nothing to persist; the site stays throttled and the
		// operator can re-run `rabbot verify <site>` later.
		return
	}

	// Verified: record the config INTENT (with VerifiedAt) — the authoritative
	// init-time record the daemon re-verifies. The verified method comes from the
	// outcome's ProofRecord (the method the operator actually picked + passed),
	// never a fixed guess. writeVerificationIntent surfaces a drift note to stdout
	// if the site is somehow absent from config.yaml.
	ew := &errWriter{w: out}
	if werr := writeVerificationIntent(ew, cfgPath, siteURL, config.VerificationConfig{
		Method:     string(outcome.Record.Method),
		Token:      token,
		VerifiedAt: outcome.Record.VerifiedAt.UTC().Format(time.RFC3339),
	}); werr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "could not record verification (config is written): %v\n", werr)
		return
	}

	// Best-effort DB proof record: the just-started daemon may not have inserted
	// the site row yet, so a not-found / open failure is skipped silently — the
	// daemon re-verifies the living state. The config intent above is the
	// authoritative init-time record either way.
	saveVerifiedRecordBestEffort(ctx, cfgPath, siteURL, outcome.Record)
}

// saveVerifiedRecordBestEffort opens the store and writes the proof record for
// the just-verified site IF its row already exists. It is deliberately advisory:
// at post-go-live the daemon has only just started, so the site may not be in
// the DB yet, and the daemon re-verifies the living state regardless (spec §E).
// Any failure (config load, store open, site-not-found, save) is swallowed — the
// authoritative init-time record is the config intent the caller already wrote.
func saveVerifiedRecordBestEffort(ctx context.Context, cfgPath, siteURL string, rec verify.ProofRecord) {
	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		return
	}
	db, err := store.Open(ctx, databasePath(&cfg))
	if err != nil {
		return
	}
	defer func() { _ = db.Close() }()
	site, err := db.GetSiteByBaseURL(ctx, siteURL)
	if err != nil {
		return // site not in the store yet — the daemon will record it on re-verify
	}
	rec.SiteID = site.ID
	_ = db.SaveVerification(ctx, site.ID, rec)
}

// runAlertsUpgrade is the post-go-live "get notified when it changes" action
// dispatched from the upgrade menu. It re-runs the EXPLICIT alerts-channel step
// (decision #23) so the operator can add or change a channel from the menu —
// Slack / email / a generic webhook — or deliberately pick "no alerts (pull-only)".
// It shares the SAME channel choice + per-channel collectors + write path the
// required onboarding alerts gate uses (runAlertsStep), so the menu re-entry and the
// gate never diverge. A re-pick of an existing notifier name replaces it in place
// (AddNotifierYAML is idempotent by name), so re-entering never duplicates config.
//
// UNTESTED SEAM: the channel-select + per-channel inputs are huh.Form.Run calls that
// need a real terminal; the routing/state logic (runAlertsChannelChoice /
// configureAlertChannel / writeNotifierChannel) is unit-tested directly.
func runAlertsUpgrade(cmd *cobra.Command, cfgPath string) {
	runAlertsStep(cmd, cfgPath)
}

// printGrafanaSizing writes the SETTLED sizing copy (wizard.GrafanaSizingNote,
// the single source of truth) to the command's stdout BEFORE the fork choice, so
// the operator sees the real Prometheus + Grafana footprint ("roughly 512 MB —
// recommend a 2 GB box; 1 GB fits but snug") before committing to either path.
// Pure print — it never gates.
func printGrafanaSizing(cmd *cobra.Command) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "See it on a dashboard — Prometheus + Grafana")
	_, _ = fmt.Fprintln(out, wizard.GrafanaSizingNote)
	_, _ = fmt.Fprintln(out)
}

// runGrafanaUpgrade is the post-go-live "See it on a dashboard" action dispatched
// from the upgrade menu (B2 §Wizard step / criterion 9). It first prints the
// SETTLED sizing copy (printGrafanaSizing) BEFORE any choice, then a huh Select
// FORKS two ways (decision 18):
//
//   - "Let Claude set it up" (recommended) — for the VPS operator juggling SSH
//     tabs: the wizard ensures a Claude config (via the existing Connect-Claude
//     writer seam) and prints a copy-paste handoff pointing the agent at
//     `rabbot observability init` and docs/observability-with-claude.md. It writes
//     NO bundle and sets NO metrics.addr — the agent runs the generator and docker
//     on the host; Rabbot's MCP stays read-only.
//   - "Do it now (technical path)" — runs the SAME generator inline (the bundle
//     writes + metrics.addr set + next-step prints, identical bytes), for the
//     operator who wants the one command immediately.
//
// The fork is the only TTY seam; the per-arm side-effects live in the pure,
// unit-tested applyGrafanaUpgrade. A fork abort (Ctrl-C/Esc) is a quiet skip — the
// operator is already live and observability is entirely optional.
//
// UNTESTED SEAM: the huh.Form.Run fork Select needs a real terminal, so it is
// exercised only by an integration `rabbot init`; the sizing print and both arm
// behaviours (applyGrafanaUpgrade) are unit-tested directly.
func runGrafanaUpgrade(cmd *cobra.Command, cfgPath string) {
	printGrafanaSizing(cmd)

	const (
		choiceClaude    = "claude"
		choiceTechnical = "technical"
	)
	choice := choiceClaude // recommended default
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("How would you like to set up the dashboard?").
				Description("Grafana needs a couple of steps (write the bundle, bring up "+
					"docker, restart the daemon). Let Claude do them for you, or run the one "+
					"command yourself.").
				Options(
					huh.NewOption("Let Claude set it up (recommended)", choiceClaude).Selected(true),
					huh.NewOption("Do it now (technical path)", choiceTechnical),
				).
				Value(&choice),
		),
	).WithInput(cmd.InOrStdin()).WithOutput(cmd.OutOrStdout())
	if err := form.Run(); err != nil {
		// A user abort (Ctrl-C/Esc) is a quiet skip — the user is already live and
		// the dashboard is optional; write nothing and set nothing.
		return
	}

	// Resolve the config dir from the config path so applyGrafanaUpgrade can place
	// the bundle under <config-dir>/observability/ on the technical path.
	cfgDir := filepath.Dir(cfgPath)
	applyGrafanaUpgrade(cmd, cfgDir, cfgPath, choice == choiceClaude)
}

// applyGrafanaUpgrade performs the chosen fork's side-effects — the pure,
// unit-tested core of runGrafanaUpgrade (criterion 9). claudePath selects the arm:
//
//   - claudePath true ("Let Claude set it up"): ensure a Claude config exists via
//     the Connect-Claude writer seam (emitConnectClaude), then print the copy-paste
//     handoff naming `rabbot observability init` and docs/observability-with-claude.md.
//     It writes NO observability bundle and sets NO metrics.addr — the agent runs
//     the generator + docker on the host; Rabbot's MCP stays read-only.
//   - claudePath false ("Do it now"): run the SAME shared generator seam
//     (runObservabilityInit) inline — write the bundle, set metrics.addr to the
//     loopback default (only when unset), and print the next steps (identical bytes
//     to `observability init`). A generator failure is advisory (the operator is
//     already live), surfaced to stderr, never a panic.
func applyGrafanaUpgrade(cmd *cobra.Command, cfgDir, cfgPath string, claudePath bool) {
	if claudePath {
		// Ensure a Claude config exists through the existing Connect-Claude writer
		// seam (the step-9 writer / emitConnectClaude) so the agent the operator
		// hands off to can already reach Rabbot over MCP. Default merge-write target
		// (claude-code = ./.mcp.json); the snippet carries no token, so this is safe.
		emitConnectClaude(cmd, connectOpts{target: "claude-code"})
		printGrafanaClaudeHandoff(cmd)
		return
	}
	// Technical path: run the shared generator inline (identical bytes to the CLI
	// generator and --with-grafana). Advisory on failure — never roll back the
	// already-live setup, never panic the menu.
	if err := runObservabilityInit(cmd, cfgDir, cfgPath); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "could not enable observability: %v\n", err)
	}
}

// printGrafanaClaudeHandoff writes the copy-paste handoff prompt for the
// "Let Claude set it up" arm: it names the deterministic generator command
// (`rabbot observability init`) and the agent recipe page
// (docs/observability-with-claude.md) so the operator can paste it into Claude
// Code (locally or on the VPS over SSH, the docs/install-with-claude.md pattern).
// Rabbot writes no bundle and sets no config here — the agent runs the generator
// and docker in its own shell; Rabbot's MCP stays read-only.
func printGrafanaClaudeHandoff(cmd *cobra.Command) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "Claude can set the dashboard up for you. Paste this to Claude Code "+
		"(locally, or on your server over SSH):")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "  Set up the Rabbot observability dashboard. Run `rabbot observability "+
		"init`, restart the daemon, then bring the Prometheus + Grafana stack up with "+
		"`docker compose -f <config-dir>/observability/docker-compose.observability.yml up -d` "+
		"and verify the Grafana dashboard is live. Follow docs/observability-with-claude.md.")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Rabbot won't run docker for you — the agent does, in its own shell. "+
		"See docs/observability-with-claude.md for the full recipe.")
}

// promptSlackWebhook collects the Slack Incoming-Webhook URL with a MASKED huh input
// (EchoModePassword), so the secret is never echoed to the screen. It prints the
// public "show me how" docs hint first (no secret), then returns the trimmed value;
// an operator abort (Ctrl-C/Esc) or an empty entry both yield "" so the caller
// treats it as a clean skip (applyAlertsStep is a no-op on ""). The collected value
// is NEVER printed by this helper.
//
// UNTESTED SEAM: the huh.Form.Run call requires a real terminal, so it is exercised
// only by an integration run; the ROUTING into it is what the alerts flow exercises.
func promptSlackWebhook(cmd *cobra.Command) string {
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), wizard.SlackDocsHint)
	var webhook string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title(wizard.SlackWebhookPrompt).
				Description("Starts with https://hooks.slack.com/services/… — we keep it private and never print it back.").
				Placeholder("https://hooks.slack.com/services/…").
				EchoMode(huh.EchoModePassword).
				Value(&webhook),
		),
	).WithInput(cmd.InOrStdin()).WithOutput(cmd.OutOrStdout())
	if err := form.Run(); err != nil {
		// A user abort (Ctrl-C/Esc) is a quiet skip, not a failure — alerts are
		// entirely optional and the user is already live.
		return ""
	}
	return strings.TrimSpace(webhook)
}

// fineTuneCustomLabel is the sentinel Select option that, when chosen, reveals the
// raw min/max duration inputs for experts. It is intentionally distinct from any
// wizard.CadenceLabels() entry so parseCadence never matches it.
const fineTuneCustomLabel = "Custom… (set exact intervals)"

// runFineTuneUpgrade is the post-go-live "fine-tune" (cadence) action dispatched
// from the upgrade menu (spec §menu "Fine-tune"). It replaces the old Go-duration
// inputs with a friendly Select — "A few times a day / About hourly / A few times
// an hour / Custom…" — mapping the chosen label to (min,max) recheck intervals via
// the pure wizard.CadenceIntervals helper, then writing them to the GLOBAL
// defaults through the SAME config writer setup.Apply uses (config.SetKeyYAML on
// defaults.min_interval / defaults.max_interval). "Custom…" reveals raw duration
// inputs for experts (validated as positive Go durations) so nothing is lost.
//
// A write failure is advisory — the operator is already live and throttled — so it
// surfaces a fix-it line on stderr but never fails the (already-persisted) setup.
//
// UNTESTED SEAM: the two huh.Form.Run calls need a real terminal, so this is
// exercised only by an integration `rabbot init`; the cadence mapping it
// composes (wizard.CadenceIntervals / CadenceLabels / parseCadence) is unit-tested.
func runFineTuneUpgrade(cmd *cobra.Command, cfgPath string) {
	out := cmd.OutOrStdout()

	// Build the friendly Select from the wizard's single source of truth, plus the
	// "Custom…" sentinel for experts. Default to the first (default) label.
	labels := wizard.CadenceLabels()
	choice := labels[0]
	opts := make([]huh.Option[string], 0, len(labels)+1)
	for _, l := range labels {
		opts = append(opts, huh.NewOption(l, l))
	}
	opts = append(opts, huh.NewOption(fineTuneCustomLabel, fineTuneCustomLabel))

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("How often should we check your site?").
				Description("This sets how long a site may go between rechecks. You can change it any time.").
				Options(opts...).
				Value(&choice),
		),
	).WithInput(cmd.InOrStdin()).WithOutput(out)
	if err := form.Run(); err != nil {
		// A user abort (Ctrl-C/Esc) is a quiet skip — fine-tuning is optional and the
		// user is already live; leave the configured cadence untouched.
		return
	}

	var minInterval, maxInterval string
	if choice == fineTuneCustomLabel {
		var ok bool
		minInterval, maxInterval, ok = promptCustomCadence(cmd)
		if !ok {
			// Backed out of the custom inputs: a clean skip, write nothing.
			return
		}
	} else {
		c, _, parsed := wizard.ParseCadence(choice)
		if !parsed {
			// Defensive: an option we offered should always parse. If it somehow does
			// not, skip rather than write a guessed cadence.
			return
		}
		minInterval, maxInterval = wizard.CadenceIntervals(c)
	}

	// Persist to the GLOBAL defaults via the same writer setup.Apply uses, so the
	// fine-tune path and the headless/setup path share one writer. A write failure is
	// advisory (config is already written + live); surface it, never fail setup.
	if err := config.SetKeyYAML(cfgPath, "defaults.min_interval", minInterval); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "could not update how often we check (config is written): %v\n", err)
		return
	}
	if err := config.SetKeyYAML(cfgPath, "defaults.max_interval", maxInterval); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "could not update how often we check (config is written): %v\n", err)
		return
	}
	_, _ = fmt.Fprintf(out, "Cadence updated — we'll check between every %s and %s.\n", minInterval, maxInterval)
}

// promptCustomCadence reveals the raw min/max recheck-interval inputs for experts
// who picked "Custom…". Each value must be a positive Go duration (validated inline
// the same way the wizard's scope step validates intervals); an operator abort
// (Ctrl-C/Esc) returns ok=false so the caller treats it as a clean skip and writes
// nothing.
//
// UNTESTED SEAM: the huh.Form.Run call needs a real terminal; the validation it
// enforces (positive Go duration) mirrors the wizard's validateInterval contract.
func promptCustomCadence(cmd *cobra.Command) (minInterval, maxInterval string, ok bool) {
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Minimum time between checks").
				Description("A Go duration like 10m or 1h — never check more often than this.").
				Placeholder("10m").
				Validate(validatePositiveDuration).
				Value(&minInterval),
			huh.NewInput().
				Title("Maximum time between checks").
				Description("A Go duration like 1h or 24h — always check at least this often.").
				Placeholder("24h").
				Validate(validatePositiveDuration).
				Value(&maxInterval),
		),
	).WithInput(cmd.InOrStdin()).WithOutput(cmd.OutOrStdout())
	if err := form.Run(); err != nil {
		return "", "", false
	}
	return strings.TrimSpace(minInterval), strings.TrimSpace(maxInterval), true
}

// validatePositiveDuration is the fine-tune custom-input validator: the value must
// be a Go duration time.ParseDuration accepts AND be strictly positive, so a typo
// like "1hour" or a "0s"/"-5m" cannot be written to config.yaml and then silently
// fall back to the default at runtime (config.durOr). It mirrors the wizard scope
// step's validateInterval, but rejects empty (a custom value is required here).
func validatePositiveDuration(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("enter a Go duration like 10m or 24h")
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return errors.New("use a Go duration like 10m or 24h")
	}
	if d <= 0 {
		return errors.New("must be a positive duration")
	}
	return nil
}

// ensureWizardInstanceKey creates the data dir (DataDirPath is a pure resolver
// that does NOT, and the wizard is the first thing run on a fresh install) and
// then loads or mints the per-instance key. It passes cfg.DataDir as the override
// — the SAME value instanceKeyPath(&cfg) uses — so the created dir and the key
// path always agree even under a data_dir override. Without the ResolveDataDir
// step the key write ENOENTs on first run and the wizard never starts.
func ensureWizardInstanceKey(cfg config.Config) ([]byte, error) {
	if _, err := config.ResolveDataDir(cfg.DataDir); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return verify.LoadOrCreateInstanceKey(instanceKeyPath(&cfg))
}

// slackConfiguredFromConfig reports whether the written config has any notifier,
// without ever reading the (secret) URL. Used by the wizard runner to set the
// summary's SlackConfigured flag from disk rather than threading the webhook.
func slackConfiguredFromConfig(cfgPath string) bool {
	loaded, err := config.Load(cfgPath, nil)
	if err != nil {
		return false
	}
	return len(loaded.Notifiers) > 0
}

// persistWizardResult assembles the setup.Plan from the wizard's pure Inputs,
// writes it to cfgPath, and records per-site proof-of-control intent. It has no
// TTY dependency (it takes already-collected wizard.Inputs); errOut receives the
// advisory Connect-Claude write feedback (use io.Discard to suppress it), so the
// whole post-collection persistence — including the verification-block write — is
// unit-testable independently of the interactive seam.
func persistWizardResult(cmd *cobra.Command, in wizard.Inputs, cfgPath string, bi BuildInfo, errOut io.Writer) (setup.Result, error) {
	plan, err := wizard.BuildPlan(in)
	if err != nil {
		return setup.Result{}, err
	}

	// Seed the commented scaffold when there is no file yet, so the
	// comment-preserving writers produce a readable, fully-structured config.
	if _, statErr := os.Stat(cfgPath); os.IsNotExist(statErr) {
		if werr := os.WriteFile(cfgPath, []byte(defaultConfigYAML), 0o600); werr != nil {
			return setup.Result{}, werr
		}
	}
	if _, derr := config.ResolveDataDir(""); derr != nil {
		return setup.Result{}, derr
	}

	res, err := plan.Apply(setup.Options{ConfigPath: cfgPath, Version: bi.Version, Now: in.AttestedAt})
	if err != nil {
		return setup.Result{}, err
	}

	// Record per-site proof-of-control intent. There are two reachable outcomes
	// from the wizard's proof screen (verify.Verify only returns verified or
	// throttled): a VERIFIED site writes Method+Token+VerifiedAt; a THROTTLED
	// (verify-miss) site still writes Method+Token (no VerifiedAt) so the token the
	// operator was just shown placement instructions for survives — a later
	// `rabbot verify` reuses it instead of minting a fresh one and orphaning the
	// proof already placed. This mirrors the CLI twin (internal/cli/verify.go),
	// which writes the intent before the check runs. A site with no token (nothing
	// placed) gets no block.
	for _, s := range in.Sites {
		if s.Token == "" {
			continue
		}
		vc := config.VerificationConfig{
			Method: string(s.Method),
			Token:  s.Token,
		}
		if s.Verified && !s.VerifiedAt.IsZero() {
			vc.VerifiedAt = s.VerifiedAt.UTC().Format(time.RFC3339)
		}
		found, werr := setSiteVerificationFn(cfgPath, s.URL, vc)
		if werr != nil {
			return setup.Result{}, fmt.Errorf("write verification block for %s: %w", s.URL, werr)
		}
		// Apply just wrote this site, so the row must exist. A false here means the
		// just-earned proof intent was silently dropped (leaving the site with no
		// recorded token, contrary to spec §E) — surface it instead.
		if !found {
			return setup.Result{}, fmt.Errorf("verification block target site %s not found in config", s.URL)
		}
	}

	// Spec B — write each site's coverage-cap choice post-Apply. The cap is OUT-OF-BAND
	// (setup.Apply's AddSiteYAML writes no discovery block), so we set it per-site via
	// config.SetSiteMaxPagesYAML keyed by URL — NEVER SetKeyYAML (which cannot index the
	// sites sequence). A nil MaxPages means "keep the resolved default" — no write. 0 =
	// unlimited; N = cap at N. The *int round-trips through ResolveDiscovery
	// (nil=inherit→2000, 0=unlimited, N=N) with no resolver change.
	for _, s := range in.Sites {
		if s.MaxPages == nil {
			continue
		}
		if werr := config.SetSiteMaxPagesYAML(cfgPath, s.URL, *s.MaxPages); werr != nil {
			return setup.Result{}, fmt.Errorf("write page cap for %s: %w", s.URL, werr)
		}
	}

	// Step 8 — alerts (optional): write the Slack notifier + best-effort test alert,
	// reusing the SAME helper + seam as the headless path. A config-write failure
	// aborts (spec error-handling); the send is advisory. The webhook is never
	// printed; it flows verbatim into the notifier (${ENV}-safe).
	if err := applyAlertsStep(cmd, cfgPath, in.SlackWebhook); err != nil {
		return setup.Result{}, err
	}

	// Connect-Claude (step 9): advisory and NON-FATAL — write the chosen MCP host
	// config if the user opted in. The proof/verification work above is already
	// persisted, so a connect-write failure must not fail setup. (mirrors how
	// alerts are advisory.) The outcome is surfaced to errOut via reportConnectWrite
	// — the same feedback the headless emitConnectClaude prints — so a wizard user
	// who chose "Write ./.mcp.json" gets the written path (or a non-fatal warning),
	// not silence. The config carries no token/env, so printing the path is safe.
	//
	// DIR-COHERENCE PARITY: bake the daemon's NON-DEFAULT data_dir into the written
	// snippet exactly as the headless emitConnectClaude path does, so a wizard user
	// on a custom dir gets a launch entry the mcp child can use to find the daemon
	// (and not a broken default-dir snippet). We reload the effective config (env >
	// file) to read DataDir; the load error is non-fatal (connect is advisory) and
	// leaves DataDir "" → the byte-identical legacy snippet via the connectWriteFn
	// seam the tests rely on. A default data_dir is "" so only a custom dir activates
	// the dirs writer (mirrors emitConnectClaude's local branch + headless rationale).
	if in.ConnectMCP && in.ConnectTarget != "" && in.ConnectTarget != "print" {
		loaded, _ := config.Load(cfgPath, nil)
		dataDir := loaded.DataDir
		var (
			path string
			werr error
		)
		if dataDir != "" {
			tgt, perr := mcpsrv.ParseTarget(in.ConnectTarget)
			if perr != nil {
				werr = perr
			} else if path, werr = mcpsrv.TargetPath(tgt); werr == nil {
				werr = connectWriteDirs(path, mcpsrv.ResolveBinary(), dataDir, "")
			}
		} else {
			// Default dir: keep the existing seam (tests assert connectWriteFn here).
			path, werr = connectWriteFn(in.ConnectTarget)
		}
		reportConnectWrite(errOut, in.ConnectTarget, path, werr)
	}

	// Step 10 — run it (offer): install service / start daemon, reusing the SAME
	// helper + seams as the headless path. Both advisory; never roll back config.
	applyRunServiceStep(cmd, bi, in.InstallService, in.StartDaemon)

	return res, nil
}
