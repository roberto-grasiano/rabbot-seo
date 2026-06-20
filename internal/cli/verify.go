package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/humanize"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// verifyDeps carries everything runVerify needs, so the core is testable in
// isolation (mirroring runDoctor). Production wires the real store/config and
// allowPrivate=false; tests inject a loopback httptest server via baseOverride +
// allowPrivate=true and a fixed clock.
type verifyDeps struct {
	db           *store.DB
	configPath   string
	cfg          *config.Config
	target       string        // the site URL or id as the operator typed it
	method       verify.Method // which proof surface to use
	key          []byte        // the per-instance secret key; the token is DERIVED from it
	skip         bool          // --skip: record an attestation, stay throttled
	allowPrivate bool          // test-only: clear the SSRF guard for loopback
	baseOverride string        // test-only: hit an httptest base instead of https://<host>
	now          time.Time     // injected clock
	// client, when non-nil, is the loopback control client. When the daemon is up
	// (Health == nil), the verify-now DB write routes through client.Verify so the
	// daemon stays the single DB writer (D6). When the daemon is down
	// (ErrDaemonNotRunning), runVerify falls back to the direct store write. Tests
	// inject an httptest-backed client; production wires newControlClient.
	client *control.Client
}

// runVerify proves control of a site and records BOTH the config verification
// block (INTENT, comment-preserving) AND the DB proof record (the authoritative
// living state the Phase 4 daemon re-verifies). It opens the store directly (not
// the control client) so it works whether or not the daemon is running. It is
// the testable core of the verify command.
//
// SCOPE GUARD (Phase 3): NO throttle/rate resolver and NO daemon re-verify loop
// — those are Phase 4.
func runVerify(ctx context.Context, w io.Writer, d verifyDeps) error {
	ew := &errWriter{w: w}

	// Resolve the site row by its base URL (the CLI passes the URL; sites are
	// keyed by base_url). This gives us the DB id for the proof record.
	site, err := d.db.GetSiteByBaseURL(ctx, d.target)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("site not monitored: %s (add it first with `rabbot sites add`)", d.target)
		}
		return err
	}

	host := hostFromURL(site.BaseURL)
	// The token is DERIVED from this instance's secret key bound to the host — it
	// is never caller-supplied. Re-derivation is deterministic, so a re-run shows
	// the same token, and another instance (different key) derives a different one,
	// so a replayed surface or a hand-edited config can never fake a verified state.
	token := verify.DeriveToken(d.key, host)

	// ── Skip path: record an attestation, keep the site throttled ────────────
	if d.skip {
		// The skip-to-attested bypass REQUIRES the prior Step-2 authorization
		// attestation, so it is a deliberate recorded act, not a silent default.
		if d.cfg.Setup.AttestedAt == "" {
			return errors.New("cannot skip verification: no authorization attestation on record " +
				"(run `rabbot init` / setup and attest first)")
		}
		// Write the config intent FIRST (mirroring the verify-now path), then the DB
		// record. The token is DISPLAY/AUDIT only on both — the daemon re-verifies by
		// re-deriving, so this stored token is never trusted as proof.
		if err := writeVerificationIntent(ew, d.configPath, site.BaseURL, config.VerificationConfig{
			Method: string(d.method),
			Token:  token,
		}); err != nil {
			return err
		}
		rec := verify.Attest(site.ID, d.method, d.now)
		// Attest leaves Token zero; persist the derived token here so the operator
		// sees the same value when they come back to place it.
		rec.Token = token
		if err := d.db.SaveVerification(ctx, site.ID, rec); err != nil {
			return fmt.Errorf("save attestation: %w", err)
		}
		ew.printf("Verification SKIPPED for %s (attested).\n", site.BaseURL)
		ew.println("The site stays THROTTLED until a successful verify.")
		ew.println("To lift the throttle later, place the proof and run `rabbot verify` again.")
		printPlacement(ew, d.method, host, token)
		return ew.err
	}

	// ── Verify-now path ──────────────────────────────────────────────────────
	ew.printf("Proof token for %s:\n", site.BaseURL)
	printPlacement(ew, d.method, host, token)
	ew.println("")

	// Persist the method+token INTENT before the check runs (display/audit only —
	// the daemon re-verifies by DERIVING, so this token is never trusted as proof).
	// The success branch below re-writes this block additionally recording
	// VerifiedAt.
	if err := writeVerificationIntent(ew, d.configPath, site.BaseURL, config.VerificationConfig{
		Method: string(d.method),
		Token:  token,
	}); err != nil {
		return err
	}

	// ── Verify-now persistence: prefer the daemon (single-writer) when it is up ──
	// D6: if the daemon is running it owns the DB; route the check through the
	// endpoint so the MCP path and this CLI invocation never both write. Fall back
	// to the direct store write only when the daemon is down.
	if d.client != nil {
		if herr := d.client.Health(ctx); herr == nil {
			resp, cerr := d.client.Verify(ctx, control.VerifyRequest{
				SiteID: site.ID,
				Method: string(d.method),
				Action: "check",
			})
			if cerr != nil {
				return fmt.Errorf("verify via daemon: %w", cerr)
			}
			// On a daemon-routed promotion the daemon has only written the proof
			// record — the live frontier still carries the old unverified throttle
			// until the next reconcile (~hourly) or a manual reload. Trigger the
			// existing control reload so reconcileSites + installThrottleFloors run
			// now and the host's rate actually drops live, making the "FULL SPEED"
			// copy truthful. Best-effort: the proof is already persisted, so a reload
			// failure must not fail the command — the copy is softened instead.
			reloaded := func() bool {
				if verify.State(resp.State) != verify.StateVerified {
					return false
				}
				return d.client.Reload(ctx) == nil
			}()
			return reportVerifyResult(ew, site.BaseURL, d.method, d.configPath, token,
				verify.State(resp.State), verify.Reason(resp.Reason), reloaded)
		} else if !errors.Is(herr, control.ErrDaemonNotRunning) {
			// A token mismatch (401) or other non-down error is a real failure — do
			// NOT silently fall through to a direct write that would split the writer.
			return fmt.Errorf("verify: control plane unreachable: %w", herr)
		}
		// daemon down: fall through to the direct write below.
	}

	out, verr := verify.Check(ctx, d.db, site.ID, host, d.method, verify.Options{
		Now:          d.now,
		AllowPrivate: d.allowPrivate,
		BaseOverride: d.baseOverride,
		Key:          d.key,
	})
	if verr != nil {
		ew.printf("Could not verify %s: %v\n", site.BaseURL, verr)
		ew.println("The site remains THROTTLED.")
		return ew.err
	}
	// Direct-write path: the daemon is down (or no control client), so there is
	// no live frontier to re-rate right now — the new crawl rate takes effect the
	// next time the daemon starts/reconciles. reloaded=false keeps the copy honest.
	return reportVerifyResult(ew, site.BaseURL, d.method, d.configPath, token, out.Record.State, out.Reason, false)
}

// reportVerifyResult renders the verified/miss outcome and, on success, rewrites
// the config verification block additionally recording VerifiedAt. It is shared by
// the daemon-routed and direct-write paths. reloaded reports whether the live
// frontier rate was actually re-applied (only the daemon-routed path can trigger a
// reload, and only when it succeeds): when true the new rate is already live, so
// the copy promises FULL SPEED now; when false the proof is recorded but the live
// rate only widens on the next reconcile, so the copy says so rather than claiming
// an instant change that did not happen.
func reportVerifyResult(ew *errWriter, baseURL string, method verify.Method, configPath, token string, state verify.State, reason verify.Reason, reloaded bool) error {
	if state == verify.StateVerified {
		if err := writeVerificationIntent(ew, configPath, baseURL, config.VerificationConfig{
			Method:     string(method),
			Token:      token,
			VerifiedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
		ew.printf("Verified %s via %s.\n", baseURL, method)
		if reloaded {
			ew.println("The site now runs at FULL SPEED.")
		} else {
			// The proof is recorded and the throttle is lifted, but no live daemon
			// re-rated the frontier just now — be honest that FULL SPEED applies on
			// the next reconcile rather than claiming an instant change that did not
			// happen. (Keeps the "full speed" phrasing so operators see the payoff.)
			ew.println("The throttle is lifted — the site will run at FULL SPEED on the next reconcile " +
				"(the next periodic re-verify, a config change, or a SIGHUP reload of a running daemon).")
		}
		return ew.err
	}
	ew.printf("NOT VERIFIED: %s (%s) — the proof was not found via %s.\n", baseURL, reason, method)
	ew.println("The site remains THROTTLED until the proof is in place and verify succeeds.")
	return ew.err
}

// writeVerificationIntent writes the config verification (INTENT) block for
// baseURL and surfaces config drift: SetSiteVerificationYAML reports found=false
// (and writes nothing) when the site is in the DB but absent from config.yaml
// (hand-edit, a non-config add path, or a reset config). Discarding that bool
// would let runVerify print "Verified … FULL SPEED" while the intent block never
// landed, so emit a note instead. The DB proof record (the authoritative living
// state) is still written by the caller regardless.
func writeVerificationIntent(ew *errWriter, configPath, baseURL string, v config.VerificationConfig) error {
	found, err := config.SetSiteVerificationYAML(configPath, baseURL, v)
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if !found {
		ew.printf("note: %s not found in config.yaml; intent not recorded — "+
			"run `rabbot sites add` to register it.\n", baseURL)
	}
	return nil
}

// hostFromURL extracts the host[:port] from a site base URL for DISPLAY/proof
// placement. It delegates to the shared humanize.DisplayHost so the CLI and wizard
// agree on the exact host[:port] string (DISTINCT from urlx.Host, which strips the
// port for host-equality comparison). On a parse failure it falls back to the raw
// value (ValidateSiteURL has already gated the input).
func hostFromURL(raw string) string {
	return humanize.DisplayHost(raw)
}

// printPlacement prints method-specific instructions for placing the proof.
func printPlacement(ew *errWriter, method verify.Method, host, token string) {
	switch method {
	case verify.MethodWellKnown:
		ew.printf("  Place this file:  https://%s/.well-known/rabbot-verify.txt\n", host)
		ew.printf("  With contents:    %s\n", token)
	case verify.MethodDNS:
		// DNS resolves names, not host:port pairs — VerifyDNS strips the port via
		// url.URL.Hostname(), so the hint must show the same bare hostname or it
		// would tell the operator to add the record on "example.com:8443" while the
		// lookup targets "example.com". The HTTP-fetch branches keep the full host.
		ew.printf("  Add a DNS TXT record on %s:\n", (&url.URL{Host: host}).Hostname())
		ew.printf("    rabbot-verify=%s\n", token)
	case verify.MethodMeta:
		ew.printf("  Add to the <head> of https://%s/:\n", host)
		ew.printf("    <meta name=\"rabbot-verify\" content=\"%s\">\n", token)
	}
}

// parseMethod maps the --method flag to a verify.Method, defaulting to
// well_known and rejecting unknown values.
func parseMethod(s string) (verify.Method, error) {
	switch s {
	case "", "well_known":
		return verify.MethodWellKnown, nil
	case "dns":
		return verify.MethodDNS, nil
	case "meta":
		return verify.MethodMeta, nil
	default:
		return "", fmt.Errorf("unknown --method %q (want well_known|dns|meta)", s)
	}
}

// newVerifyCmd builds `rabbot verify <url|site>`. It proves control of a site
// and lifts the unverified throttle, writing both the config intent and the DB
// proof record. It opens the store directly (like the read-only commands) so it
// works whether or not the daemon is running.
func newVerifyCmd() *cobra.Command {
	var methodFlag string
	var skip bool
	cmd := &cobra.Command{
		Use:   "verify <url>",
		Short: "Prove you control a site (well-known file / DNS TXT / meta tag) and lift the throttle",
		Long: "verify proves you control a monitored site by checking an instance-bound, " +
			"unguessable token you place via a .well-known file, a DNS TXT record, or a homepage " +
			"<meta> tag, then records the proof and lifts the unverified throttle. The token is " +
			"derived from this instance's secret key and re-derived on every check, so it cannot " +
			"be replayed by another instance or faked by editing config. --skip records an " +
			"attestation (requires a prior authorization attestation). The daemon re-verifies the " +
			"living state.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			// Validate the target at the boundary (like doctor): production keeps the
			// SSRF posture (allowPrivate=false); tests call runVerify directly.
			if err := fetcher.ValidateSiteURL(args[0], false); err != nil {
				return err
			}
			method, err := parseMethod(methodFlag)
			if err != nil {
				return err
			}
			cfg, err := loadConfig(c)
			if err != nil {
				return err
			}
			db, err := store.Open(c.Context(), databasePath(cfg))
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			key, err := verify.LoadOrCreateInstanceKey(instanceKeyPath(cfg))
			if err != nil {
				return fmt.Errorf("load instance key: %w", err)
			}

			dir, err := config.ResolveConfigDir()
			if err != nil {
				return err
			}
			// Build the loopback control client so the verify-now write can route
			// through the daemon when it is up (D6). A client-build failure (no
			// token yet) leaves client nil -> the direct-write fallback runs.
			var ctrlClient *control.Client
			if cc, cerr := newControlClient(cfg); cerr == nil {
				ctrlClient = cc
			}
			return runVerify(c.Context(), c.OutOrStdout(), verifyDeps{
				db:         db,
				configPath: config.ConfigFilePath(dir),
				cfg:        cfg,
				target:     args[0],
				method:     method,
				key:        key,
				skip:       skip,
				client:     ctrlClient,
				// Production: SSRF guard ON, no base override, real resolver, real clock.
				allowPrivate: false,
				now:          time.Now().UTC(),
			})
		},
	}
	cmd.Flags().StringVar(&methodFlag, "method", "well_known", "proof method: well_known|dns|meta")
	cmd.Flags().BoolVar(&skip, "skip", false, "record an attestation and keep the site throttled (requires a prior authorization attestation)")
	return cmd
}
