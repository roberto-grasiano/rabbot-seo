package wizard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/charmbracelet/huh"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/humanize"
	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
	"github.com/roberto-grasiano/rabbot-seo/internal/setup"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// ErrCancelled is the sentinel Run returns when the operator aborts the wizard
// (Ctrl-C / Esc on a huh form, or an external cancellation that kills a live
// bubbletea screen). Aborting an interactive onboarding wizard is a normal,
// expected action — the cli layer maps this to a clean, quiet exit (no failure
// banner, no usage dump). No config is written until Run succeeds, so a cancel
// never leaves a corrupt or partial config behind.
var ErrCancelled = errors.New("setup cancelled")

// isUserCancel reports whether err represents an operator-initiated abort rather
// than a real failure. huh.Form.Run returns huh.ErrUserAborted on Ctrl-C/Esc;
// bubbletea Program.Run returns tea.ErrProgramKilled (possibly wrapped with the
// context error) when an external context cancels a live screen. Both are wrapped
// with %w, so errors.Is is the correct test.
func isUserCancel(err error) bool {
	return errors.Is(err, huh.ErrUserAborted) || errors.Is(err, tea.ErrProgramKilled)
}

// authorizationDeclined reports whether the operator declined the intro
// authorization attestation. The huh Confirm's Negative("Cancel") sets authorized
// to false WITHOUT returning huh.ErrUserAborted, so the intro.Run error guard does
// not catch it; this predicate lets Run short-circuit before the expensive live
// screens (matching the "Cancel" label) instead of running everything only for
// BuildPlan to reject an unauthorized plan at the very end. Extracted as a pure
// predicate so the guard is unit-tested directly; intro.Run is the untested seam.
func authorizationDeclined(authorized bool) bool { return !authorized }

// screenCancelled reports whether a live bubbletea screen was quit (Ctrl-C / Esc)
// before it completed. tea.Quit is the SAME path a screen takes to self-quit on
// completion, so Program.Run returns (model, nil) in both cases; the model's done
// flag (set only when the screen records its result) is the only signal that
// distinguishes "user bailed" (done=false) from "finished" (done=true). Extracted
// as a pure predicate so the guard logic is unit-tested directly; the Program.Run
// call that produces the model is the untested TTY seam.
func screenCancelled(done bool) bool { return !done }

// cancel renders the friendly abort line to d.Out and returns the ErrCancelled
// sentinel, so an operator abort reads as "setup cancelled." instead of a bare
// error + usage dump. It is the single place Run funnels user-cancel errors.
func (d Deps) cancel(err error) error {
	if d.Out != nil {
		_, _ = fmt.Fprintln(d.Out, "setup cancelled.")
	}
	// Preserve the underlying cause for callers that want it (errors.Is still
	// matches ErrCancelled); the cli layer keys off ErrCancelled for a quiet exit.
	return fmt.Errorf("%w: %w", ErrCancelled, err)
}

// Deps carries every collaborator the wizard needs, all as injectable seams so
// the orchestration is wired the same in production and (where exercised) in
// tests. The interactive huh.Form.Run call in Run is the ONLY untested part (it
// requires a real terminal); every pure helper (BuildPlan, the copy builders,
// and validateContact / validateSite) is unit-tested.
//
// The Precheck / Verify / Derive seams are NOT called by Run anymore — the
// essential path collects only contact/site/attestation. They remain on Deps
// because the runner consumes them OUTSIDE Run: Precheck drives the go-live
// verdict, and Verify / Derive drive the opt-in verification step on the
// post-go-live upgrade menu.
type Deps struct {
	In       io.Reader
	Out      io.Writer
	Version  string
	Defaults config.Config

	// Precheck performs the LIVE precheck for a URL (production: precheck.Run
	// with the resolved Options). Consumed by the runner at go-live, not by Run.
	Precheck func(ctx context.Context, url string) (precheck.Report, error)
	// Verify performs the LIVE proof-of-control check (production: verify.Verify).
	// Consumed by the upgrade menu's verification step, not by Run.
	Verify func(ctx context.Context, host string, method verify.Method) (verify.Outcome, error)
	// Derive mints the instance-bound proof token to display for a host
	// (production: a closure over verify.DeriveToken bound to the instance key).
	// Consumed by the upgrade menu's verification step, not by Run.
	Derive func(host string) string
	// Now is the injected clock for the attestation timestamp.
	Now func() time.Time

	// CountPages resolves a sitemap page count for a site URL behind an injectable
	// seam so the cap step (Phase 4) branches deterministically without touching the
	// network in tests. ok is true with the counted pages when a usable sitemap was
	// read; (0, false) signals a missing/broken/slow sitemap → the ranged-question
	// fallback. Production wires cli.productionCountPages (a closure over
	// cli.countSitemapPages with allowPrivate=false). The wizard backgrounds the call
	// and ctx-cancels it when Run returns (see startCount), so it never blocks init
	// exit; this seam itself adds no goroutine.
	CountPages func(ctx context.Context, url string) (int, bool)
}

// Run drives the interactive essential path and returns the collected Inputs
// with ONLY the contact identity, the authorization attestation, and the first
// site populated. The flow is a single payoff-first huh form in plain language:
// a welcome note → "what site?" (with a skippable staging nudge) → "you're
// allowed?" (attestation phrased against that site) → "who's monitoring?" (the
// contact EMAIL, with a live, jargon-free identity preview of the resulting UA).
//
// Everything that used to live inline here — the precheck/proof live screens and
// the scope/connect/alerts/run forms — has LEFT the linear flow by design: the
// precheck runs at go-live (in the runner), and the rest are now opt-in items on
// the post-go-live upgrade menu. Run therefore returns a single SiteDraft with
// just the URL (no proof yet — verification happens from the menu); the menu and
// go-live wiring fill in the remaining Inputs fields. The Precheck/Verify/Derive
// Deps seams stay on the struct because the menu and go-live consume them, but
// Run no longer calls them.
//
// An operator abort (Ctrl-C / Esc on the huh form) is funnelled through d.cancel
// and surfaces as the ErrCancelled sentinel + a friendly "setup cancelled." line,
// so the cli layer can exit quietly instead of printing a failure banner. No
// config is written until Run returns successfully, so a cancel is always clean.
//
// UNTESTED SEAM: the huh.Form.Run call below requires a real terminal, so it is
// exercised only by an integration run (a TTY `rabbot init`), never by unit
// tests. Everything it orchestrates — the copy builders, BuildPlan, the
// validate/preview helpers, and isUserCancel / Deps.cancel — is unit-tested in
// isolation.
func Run(ctx context.Context, d Deps) (Inputs, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// The cap step's background count is owned by a child context cancelled when Run
	// returns, so the goroutine can never outlive the form or block init exit. We also
	// collect each started count's done channel and drain them on the way out so a
	// just-launched goroutine is fully reaped before Run returns.
	//
	// countDones INVARIANT: it is appended to ONLY from startCapCount, which runs on the
	// huh/bubbletea render loop (the DescriptionFunc trigger) DURING intro.Run; it is
	// ranged over ONLY in the defer below, AFTER intro.Run has returned and ccancel has
	// fired. Those two phases never overlap in the happy path — but an externally
	// cancelled live screen can leave a render-goroutine momentarily appending while the
	// defer drains, so both the append and the range are guarded by countDonesMu to keep
	// the slice race-clean (CLAUDE.md: race-clean is a gate).
	cctx, ccancel := context.WithCancel(ctx)
	var (
		countDonesMu sync.Mutex
		countDones   []<-chan struct{}
	)
	defer func() {
		ccancel()
		countDonesMu.Lock()
		dones := countDones
		countDonesMu.Unlock()
		for _, ch := range dones {
			<-ch
		}
	}()

	var (
		authorized   bool
		contactEmail string
		siteURL      string
	)

	// ── Cap-step (Spec B) state ──────────────────────────────────────────────
	// The per-host rate + effective cap come from PlanCap against the loaded defaults for
	// a not-yet-configured site (the zero SiteConfig → the defaults), at StateVerified so
	// the estimate reflects the site's BEST achievable speed.
	capCfg := &d.Defaults
	var capSite config.SiteConfig
	cs := &capState{}
	var (
		rangedBucket = setup.BallparkUnder1k.Label() // default = one-keystroke dismissal
		capChoiceSel = capKeep
		capSetNText  string
	)
	// startCapCount is the render-time trigger bound into the site-URL field's
	// DescriptionFunc below; it starts the background count once per distinct URL.
	startCapCount := func(u string) {
		if ch := maybeStartCount(cctx, cs, u, d.CountPages); ch != nil {
			countDonesMu.Lock()
			countDones = append(countDones, ch)
			countDonesMu.Unlock()
		}
	}

	// ── The essential path: welcome → site → attest → contact ────────────────
	// Order is deliberate: the site comes FIRST so the attestation and the
	// identity preview can be phrased against the very site the operator just
	// named, keeping every screen concrete and jargon-free.
	intro := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("Welcome to Rabbot").
				Description(WelcomeText),
		),
		huh.NewGroup(
			huh.NewInput().
				Title("What site do you want to keep an eye on?").
				Placeholder("https://yoursite.com").
				Value(&siteURL).
				Validate(validateSite). // validation only — NEVER start the count here
				DescriptionFunc(func() string {
					// huh evaluates this lazily on every render. Starting the count from
					// here (not Validate) means it kicks off as soon as a syntactically
					// valid URL is on screen, and maybeStartCount's idempotent setURL gate
					// ensures exactly one goroutine per distinct URL.
					//
					// KNOWN MINOR EDGE: huh caches a DescriptionFunc's output keyed by a
					// hash of its bindings (&siteURL here) and only re-invokes the func when
					// that hash changes. On an A→B→A revert the binding hash returns to A's
					// value, so huh may serve the cached description WITHOUT re-invoking this
					// trigger — the count for A is not restarted. That is fine: cs.setURL is
					// keyed by URL, so A's already-landed result is reused as-is, and a
					// genuinely new URL always changes the hash and re-triggers. Accepted as
					// a benign edge (no stale or duplicate count results).
					if validateSite(siteURL) == nil {
						startCapCount(siteURL)
					}
					return StagingNudge
				}, &siteURL),
		),
		// ── Cap step, reading hold (Item C): while the background sitemap count is IN
		// FLIGHT (phaseCounting), show ONLY this unmistakable "⏳ Reading sitemap.xml…"
		// note and present NEITHER the ranged question NOR the cap choices. It reveals
		// off the three-state countPhase via readingGateActive, so the wizard never
		// flashes the "couldn't read a sitemap" fallback while it is still reading
		// (finding #1). The note disappears the instant the count resolves (ok/failed).
		huh.NewGroup(
			huh.NewNote().
				Title("Sizing up your site…").
				Description(ReadingSitemapNote),
		).WithHideFunc(func() bool { return !readingGateActive(cs) }),
		// ── Cap step, ranged branch (Spec B D5): shown ONLY once the count has genuinely
		// failed/timed out (phaseFailed) — never while still counting (finding #1) and
		// never on a usable count (the cap-choices step drives instead). The description
		// keeps the honest "couldn't read a sitemap" beat for this state.
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Roughly how many pages does this site have?").
				DescriptionFunc(func() string { return estimatingNote(cs) }, &siteURL).
				Options(rangedBucketOptions()...).
				Value(&rangedBucket),
		).WithHideFunc(func() bool {
			// Show the ranged ballpark question ONLY in phaseFailed (a missing/broken/slow
			// sitemap). While counting it is hidden behind the reading note above, and a
			// USABLE count (phaseOK) routes to the cap-choices group (over-cap) or asks
			// nothing at all (under-cap) — exactly the main essential flow.
			return !rangedQuestionVisible(cs)
		}),
		// ── Cap step, choices (Spec B D6): keep / monitor-all / set-N. Shown when EITHER
		// branch fires. TitleFunc/DescriptionFunc render the live engine numbers from the
		// active plan; bound to &rangedBucket so they re-render as the bucket changes.
		huh.NewGroup(
			huh.NewSelect[capChoice]().
				TitleFunc(func() string { return capStepPrompt(activeCapPlan(cs, rangedBucket, capCfg, capSite)) }, &rangedBucket).
				DescriptionFunc(func() string { return capStepAllLine(activeCapPlan(cs, rangedBucket, capCfg, capSite)) }, &rangedBucket).
				Options(
					huh.NewOption("Keep the default (recommended)", capKeep).Selected(true),
					huh.NewOption("Monitor everything", capAll),
					huh.NewOption("Set a specific number", capSetN),
				).
				Value(&capChoiceSel),
		).WithHideFunc(func() bool { return !capStepFires(cs, rangedBucket, capCfg, capSite) }),
		// ── Cap step, "set a number" (Spec B D6): shown only when "Set a specific number"
		// is picked. Validates a non-negative integer.
		huh.NewGroup(
			huh.NewInput().
				Title("How many pages should we watch?").
				Description("0 = monitor everything.").
				Placeholder("2000").
				Value(&capSetNText).
				Validate(validateMaxPagesField),
		).WithHideFunc(func() bool {
			return !capStepFires(cs, rangedBucket, capCfg, capSite) || capChoiceSel != capSetN
		}),
		huh.NewGroup(
			huh.NewConfirm().
				TitleFunc(func() string {
					return "You're allowed to monitor " + hostFromSiteURL(siteURL) + ", right?"
				}, &siteURL).
				Description("Rabbot is polite and self-identifying — confirm you have "+
					"permission to monitor this site.").
				Affirmative("Yes, I'm allowed").
				Negative("Cancel").
				Value(&authorized),
		),
		// ── Contact step (Item A): ask for an EMAIL (validated as an email), not a URL.
		// The email is published in the crawler's User-Agent so a site owner reading their
		// logs knows who is crawling and how to reach them; the live preview below shows the
		// exact identity string they will see (ContactExample), never the term "User-Agent".
		huh.NewGroup(
			huh.NewInput().
				Title("What email should site owners use to reach you?").
				Placeholder("you@example.com").
				Value(&contactEmail).
				Validate(validateContact).
				DescriptionFunc(func() string {
					// Render the realistic pre-verification per-site identity for the very
					// site the operator just named (host from siteURL) so "what owners see"
					// matches what the daemon sends. Bound to BOTH &contactEmail and &siteURL
					// so the preview re-renders as either changes; ContactExample softens to
					// the base identity if the host isn't available yet.
					return ContactExample(d.Version, contactEmail, hostFromSiteURL(siteURL))
					// huh re-evaluates when the binding's hashstructure changes; passing a
					// slice of both watched pointers (their dereferenced contents are hashed)
					// re-renders the preview as EITHER the email or the named site changes.
				}, []any{&contactEmail, &siteURL}),
		),
	).WithInput(d.In).WithOutput(d.Out)
	if err := intro.Run(); err != nil {
		if isUserCancel(err) {
			return Inputs{}, d.cancel(err)
		}
		return Inputs{}, err
	}
	// The authorization Confirm's Negative("Cancel") sets authorized=false WITHOUT
	// returning huh.ErrUserAborted, so the error guard above never sees it. Catch a
	// declined attestation here and short-circuit through d.cancel (mapped to a
	// quiet ErrCancelled exit, matching the "Cancel" label) instead of returning an
	// unauthorized plan that BuildPlan would reject downstream.
	if authorizationDeclined(authorized) {
		return Inputs{}, d.cancel(fmt.Errorf("authorization declined"))
	}

	// Resolve the cap choice into the carried *int. The step "fired" if either branch was
	// active when the form ended; if it never fired, the operator was never asked and nil
	// leaves the resolved default in place.
	fired := capStepFires(cs, rangedBucket, capCfg, capSite)
	capPtr, cerr := resolveCapDraft(fired, capChoiceSel, capSetNText)
	if cerr != nil {
		// A validated form should never reach here; never write an unvalidated cap —
		// degrade to the default (nil) rather than failing the whole setup.
		capPtr = nil
	}

	// Return only the essential collection; the post-go-live menu fills the rest
	// (proof, scope, connect, alerts, run) onto the remaining Inputs fields.
	return Inputs{
		ContactEmail: contactEmail,
		Authorized:   authorized,
		AttestedAt:   d.Now(),
		Sites:        []SiteDraft{{URL: siteURL, MaxPages: capPtr}},
	}, nil
}

// validateContact is the contact-EMAIL field validator (Item A). It reuses setup's
// contact rules (Plan.Validate → validateContactEmail) so the wizard and the headless
// path reject the same inputs: a non-email string (a URL, a bare hostname), an address
// with no "@", no domain dot, or a space, and the empty string.
func validateContact(s string) error {
	p := setup.Plan{ContactEmail: s, Authorized: true, Sites: []setup.SiteInput{{URL: "https://placeholder.example"}}}
	if err := p.Validate(); err != nil {
		// Validate checks contact first, so a contact error surfaces here; a
		// placeholder site keeps the later checks from masking it.
		return err
	}
	return nil
}

// validateSite is the site-URL field validator. It defers to
// fetcher.ValidateSiteURL (production posture: allowPrivate=false) so the wizard
// rejects non-http(s) schemes and private/loopback/metadata IP targets exactly
// like every other site-admission path.
func validateSite(s string) error {
	return fetcher.ValidateSiteURL(s, false)
}

// hostFromSiteURL extracts host[:port] from a site base URL for the proof
// placement. It delegates to the shared humanize.DisplayHost (the single
// implementation, also used by cli.hostFromURL) so the wizard and CLI agree on the
// display host; on a parse failure it returns the raw value (the field has already
// passed validateSite).
func hostFromSiteURL(raw string) string {
	return humanize.DisplayHost(raw)
}
