package cli

import (
	"fmt"
	"io"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/wizard"
)

// The post-go-live "Connect Search Console" upgrade step (menu row after "Get
// notified"). It joins the just-configured site to its Google Search Console property
// so Rabbot can read Google's own ground truth. It mirrors the alerts step's split:
// the pure routing/write core (configureGSCConnect) AND the dispatch loop
// (connectGSCUpgrade over an injectable gscPrompter) are unit-tested; the huh.Select /
// huh.Input collectors and the live doctor probe are TTY/network seams driven only by
// an integration `rabbot init`.
//
// SECRET DISCIPLINE: the step collects only a PATH to the 0600 credential file (the
// SA key or the OAuth token), never a credential body. The path is written verbatim
// via config.SetSiteGSCYAML (so an ${ENV} reference survives); the success line names
// only the property, never the path or any credential content.

// gscConnectInput is the resolved, already-collected choice configureGSCConnect acts
// on: the auth mode plus (for a connect mode) the property identifier and the single
// credential file path. Splitting collection (TTY) from the write (pure) keeps the
// write path table-testable (the alerts-step precedent).
type gscConnectInput struct {
	mode     wizard.GSCAuthMode
	property string // "https://ex.com/" OR "sc-domain:ex.com"
	credPath string // PATH to the 0600 SA key OR OAuth token file (never a body)
}

// gscPrompter is the seam through which connectGSCUpgrade collects the operator's
// choices, so the dispatch loop (mode → connect routing, abort handling, configure
// dispatch) is unit-testable without a TTY. Production is gscFormPrompter (the huh
// forms below); tests substitute a scripted prompter. Each method returns ok=false on
// an abort (Ctrl-C/Esc), which the loop treats as a quiet skip — Search Console is
// entirely optional and the operator is already live.
type gscPrompter interface {
	// AuthMode collects the auth-mode choice (service account / OAuth2 / skip).
	AuthMode() (wizard.GSCAuthMode, bool)
	// Property collects the GSC property identifier (only called for a connect mode).
	Property() (string, bool)
	// CredPath collects the PATH to the 0600 credential file for the chosen mode.
	CredPath(mode wizard.GSCAuthMode) (string, bool)
}

// gscDoctorOfferFn is the seam through which the step offers to run the doctor GSC
// connectivity/auth check after writing the block. Production points at
// offerGSCDoctorCheck (which reloads the just-written config and runs runDoctorGSC).
// Tests replace it so the unit path does no network and can assert it was offered.
var gscDoctorOfferFn = offerGSCDoctorCheck

// configureGSCConnect realizes one resolved "Connect Search Console" choice. For the
// deliberate skip (or the defensive unset) it writes nothing and prints the
// lossless-skip acknowledgment — a real terminal state, never a silent skip. For a
// connect mode it:
//  1. builds + validates the config.GSCConfig via wizard.BuildGSCConfig (property
//     shape + mode↔credential mutual-exclusion checked by the single config validator),
//  2. writes it onto the site's config.yaml block via config.SetSiteGSCYAML, and
//  3. offers the doctor connectivity check (gscDoctorOfferFn).
//
// A build/validation failure returns the error (the caller surfaces it and re-prompts
// in the live flow) and writes NOTHING — never a partial/broken block. An empty
// siteURL is a clean no-op (there is nothing to attach to). The credential path body
// is never logged or echoed.
func configureGSCConnect(cmd *cobra.Command, cfgPath, siteURL string, in gscConnectInput) error {
	out := cmd.OutOrStdout()

	if !in.mode.IsConnect() {
		// Skip (or unset, defensively): lossless — write no block, acknowledge.
		_, _ = fmt.Fprintln(out, wizard.GSCSkipAcknowledged)
		return nil
	}
	if siteURL == "" {
		// No site to attach to (the menu shouldn't reach here): clean skip, write nothing.
		_, _ = fmt.Fprintln(out, "(no site to connect yet — re-run `rabbot init` once a site is configured)")
		return nil
	}

	g, err := wizard.BuildGSCConfig(in.mode, in.property, in.credPath)
	if err != nil {
		return fmt.Errorf("connect Search Console: %w", err)
	}
	found, err := config.SetSiteGSCYAML(cfgPath, siteURL, g)
	if err != nil {
		return fmt.Errorf("write Search Console config: %w", err)
	}
	if !found {
		// The site row should exist (it was just configured); if not, say so plainly
		// rather than silently doing nothing.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "could not find %s in the config to attach Search Console to\n", siteURL)
		return nil
	}

	// Success line — names the property (non-secret), never the credential path.
	_, _ = fmt.Fprintf(out, "Search Console connected for %s (property %s).\n", siteURL, g.Property)
	if in.mode == wizard.GSCAuthOAuth {
		_, _ = fmt.Fprintln(out, "If you haven't completed the consent yet, run `rabbot gsc auth` to create the token file.")
	}

	// Offer the connectivity/auth check so the operator sees green before moving on.
	gscDoctorOfferFn(cmd, cfgPath, siteURL)
	return nil
}

// connectGSCUpgrade is the unit-tested dispatch core of the post-go-live "Connect
// Search Console" action. It prompts the auth mode through p; on a connect mode it
// collects the property + credential path, then hands the resolved choice to
// configureGSCConnect (which writes the block + offers the doctor check). An abort at
// ANY prompt is a quiet skip (return) — Search Console is optional and the operator is
// already live. A configure error is advisory (surfaced to stderr), never a crash.
// Splitting this from the huh forms (gscFormPrompter) is the alerts-step precedent:
// the routing/abort/dispatch logic is testable; only the form rendering needs a TTY.
func connectGSCUpgrade(cmd *cobra.Command, cfgPath, siteURL string, p gscPrompter) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, wizard.GSCConnectIntro)

	mode, ok := p.AuthMode()
	if !ok {
		// Abort at the mode select: quiet skip (optional step, already live).
		return
	}
	in := gscConnectInput{mode: mode}
	if mode.IsConnect() {
		property, pok := p.Property()
		if !pok {
			return
		}
		credPath, cok := p.CredPath(mode)
		if !cok {
			return
		}
		in.property = property
		in.credPath = credPath
	}
	if err := configureGSCConnect(cmd, cfgPath, siteURL, in); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "skipping the Search Console step (%v)\n", err)
	}
}

// runConnectGSCUpgrade is the production driver dispatched from the upgrade menu: it
// wires the live huh-form prompter (gscFormPrompter) to the unit-tested
// connectGSCUpgrade dispatch loop.
func runConnectGSCUpgrade(cmd *cobra.Command, cfgPath, siteURL string) {
	connectGSCUpgrade(cmd, cfgPath, siteURL, gscFormPrompter{cmd: cmd})
}

// gscDoctorProbeFn is the seam dispatchGSCDoctorCheck runs to execute the actual GSC
// connectivity/auth probe (reload the just-written config + run runDoctorGSC). Splitting
// it out lets the dispatch logic (run vs skip, the skip hint, the advisory error
// surfacing) be unit-tested with no live config or network. Production is the closure in
// offerGSCDoctorCheck.
type gscDoctorProbeFn func() error

// dispatchGSCDoctorCheck is the unit-tested core of the doctor-offer step: given the
// operator's run/skip decision it either prints the "skipped — re-run later" hint
// (run=false) or executes the probe (run=true), surfacing only a WRITE error from the
// probe as an advisory stderr line (the probe itself reports auth/reachability failures
// in its own text output; it is never fatal here). The huh.Confirm that produces `run`
// lives in offerGSCDoctorCheck (the untested TTY seam).
func dispatchGSCDoctorCheck(cmd *cobra.Command, siteURL string, run bool, probe gscDoctorProbeFn) {
	if !run {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Skipped — run `rabbot doctor "+siteURL+"` to check it later.")
		return
	}
	if err := probe(); err != nil {
		// probe only returns a WRITE error (the GSC check itself reports failures in
		// text); advisory — the operator is already configured.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "could not render the Search Console check: %v\n", err)
	}
}

// offerGSCDoctorCheck is the production doctor-offer seam: it asks (a single huh
// Confirm) whether to run the Search Console connectivity/auth check now, and dispatches
// the answer to dispatchGSCDoctorCheck. The probe reloads the just-written config and
// runs runDoctorGSC (the SAME probe `rabbot doctor <url>` uses) for the site. Declining
// (or a !TTY abort) is a clean skip. The huh.Confirm is the untested TTY seam.
func offerGSCDoctorCheck(cmd *cobra.Command, cfgPath, siteURL string) {
	run := true
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Check the Search Console connection now?").
			Description("Runs a quick read-only auth + reachability check against your property.").
			Affirmative("Yes, check it").
			Negative("Skip").
			Value(&run),
	)).WithInput(cmd.InOrStdin()).WithOutput(cmd.OutOrStdout())
	if err := form.Run(); err != nil {
		// Abort/!TTY: a clean skip — the block is written and the standalone
		// `rabbot doctor <url>` re-runs this check any time.
		return
	}
	dispatchGSCDoctorCheck(cmd, siteURL, run, func() error {
		cfg := loadWizardConfig(cfgPath)
		return runDoctorGSC(cmd.Context(), cmd.OutOrStdout(), &cfg, siteURL)
	})
}

// ── TTY seams (untested; exercised only by an integration `rabbot init`) ──────
//
// These collect operator input via huh forms. The pure write core above
// (configureGSCConnect), the dispatch loop (connectGSCUpgrade), and the doctor-offer
// dispatch (dispatchGSCDoctorCheck) are what the unit tests drive; the huh.Form.Run
// calls here need a real terminal.

// gscFormPrompter is the production gscPrompter: each method renders the corresponding
// huh form against cmd's in/out. The huh.Form.Run calls are the only untested seam; the
// option building (off the wizard single source of truth) and the ResolveGSCAuth mapping
// are exercised through the wizard package's own tests.
type gscFormPrompter struct{ cmd *cobra.Command }

func (p gscFormPrompter) AuthMode() (wizard.GSCAuthMode, bool) { return promptGSCAuthMode(p.cmd) }
func (p gscFormPrompter) Property() (string, bool)             { return promptGSCProperty(p.cmd) }
func (p gscFormPrompter) CredPath(mode wizard.GSCAuthMode) (string, bool) {
	return promptGSCCredPath(p.cmd, mode)
}

// promptGSCAuthMode presents the auth-mode choice (service account / OAuth2 / skip) as
// a single-select built from the wizard's single source of truth (GSCAuthOptions →
// ResolveGSCAuth). ok=false means the operator aborted (Ctrl-C/Esc) — the caller
// treats that as a quiet skip.
func promptGSCAuthMode(cmd *cobra.Command) (wizard.GSCAuthMode, bool) {
	opts := wizard.GSCAuthOptions()
	huhOpts := make([]huh.Option[string], 0, len(opts))
	for _, o := range opts {
		huhOpts = append(huhOpts, huh.NewOption(o.Label, o.Value))
	}
	value := opts[0].Value // default to service-account (the recommended headless path)
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("How do you want to connect Google Search Console?").
			Description("Service account is the no-browser default for a server. You can connect later by re-running `rabbot init`.").
			Options(huhOpts...).
			Value(&value),
	)).WithInput(cmd.InOrStdin()).WithOutput(cmd.OutOrStdout())
	if err := form.Run(); err != nil {
		return wizard.GSCAuthUnset, false
	}
	return resolveGSCAuthChoice(value)
}

// resolveGSCAuthChoice maps a collected auth-mode choice string to a GSCAuthMode,
// folding the wizard parser's error into the (mode, ok) shape the prompt seam returns —
// an unrecognized value is ok=false (a quiet skip), never a panic. Split out so the
// mapping is unit-tested without the huh.Select that produces the string.
func resolveGSCAuthChoice(value string) (wizard.GSCAuthMode, bool) {
	mode, err := wizard.ResolveGSCAuth(value)
	if err != nil {
		return wizard.GSCAuthUnset, false
	}
	return mode, true
}

// promptGSCProperty collects the GSC property identifier, validated by the wizard's
// pure validator (the same shape config.ValidateGSC enforces). ok=false on abort.
func promptGSCProperty(cmd *cobra.Command) (string, bool) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, wizard.GSCPropertyHelp)
	var property string
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Your Search Console property").
			Placeholder("sc-domain:whatthehellai.com").
			Value(&property).
			Validate(wizard.ValidateGSCPropertyField),
	)).WithInput(cmd.InOrStdin()).WithOutput(out)
	if err := form.Run(); err != nil {
		return "", false
	}
	return property, true
}

// promptGSCCredPath collects the PATH to the 0600 credential file for the chosen mode
// (the SA key for service_account, the OAuth token file for oauth2). It collects a
// PATH only (never a credential body) — the field is NOT masked because a filesystem
// path is not itself a secret. ok=false on abort.
func promptGSCCredPath(cmd *cobra.Command, mode wizard.GSCAuthMode) (string, bool) {
	out := cmd.OutOrStdout()
	title, placeholder := gscCredPrompt(out, mode)
	var credPath string
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title(title).
			Placeholder(placeholder).
			Value(&credPath).
			Validate(wizard.ValidateGSCKeyFileField),
	)).WithInput(cmd.InOrStdin()).WithOutput(out)
	if err := form.Run(); err != nil {
		return "", false
	}
	return credPath, true
}

// gscCredPrompt prints the mode-specific credential help to w and returns the
// (title, placeholder) for the path input. Split from promptGSCCredPath so the
// mode→copy selection (and that the right help is emitted) is unit-tested without the
// huh.Input that follows.
func gscCredPrompt(w io.Writer, mode wizard.GSCAuthMode) (title, placeholder string) {
	switch mode {
	case wizard.GSCAuthOAuth:
		_, _ = fmt.Fprintln(w, wizard.GSCOAuthCredHelp)
		return "Path to your OAuth token file (0600)", "~/.config/rabbot/gsc-oauth.json"
	default: // service account
		_, _ = fmt.Fprintln(w, wizard.GSCServiceAccountCredHelp)
		return "Path to your service-account JSON key (0600)", "/etc/rabbot/gsc-sa.json"
	}
}
