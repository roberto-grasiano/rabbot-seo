package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/notify"
	"github.com/roberto-grasiano/rabbot-seo/internal/wizard"
)

// errChannelTestFailed is a generic, secret-free placeholder used by tests to drive
// the advisory test-alert-failure path. Production never compares against it; it
// exists so a test can assert the "send failed, here's how to re-test" branch
// without a live network.
var errChannelTestFailed = errors.New("channel test alert failed")

// sendChannelTestAlertFn is the seam through which the onboarding alerts step sends
// a best-effort test alert for ANY notifier type (slack-webhook / email-smtp /
// generic-webhook). Production points at sendChannelTestAlert, which builds the
// notifier from config and sends notify.SampleTestAlert inline (the daemon is not
// up at onboarding). Tests replace it to avoid live network and to drive the
// advisory-failure path. SECRET-SAFETY: the NotifierConfig carries secrets
// (password / header values); this seam never logs them, and the errors the real
// notifiers return are already scrubbed of secrets.
var sendChannelTestAlertFn = sendChannelTestAlert

// channelTestAlertTimeout bounds the inline test alert so onboarding never stalls.
// cmd.Execute() runs with no deadline, so a pathological endpoint (repeated 429s
// driving a notifier's retry loop, or a slow SMTP dial) would otherwise block setup
// for minutes. Both the webhook backoff and the email dial honor ctx, so this
// deadline aborts a stuck attempt promptly. The test-alert is best-effort and MUST
// NOT block setup (mirrors applyAlertsStep's slack bound).
const channelTestAlertTimeout = 30 * time.Second

// writeNotifierChannel writes one configured channel to config — a notifier mapping
// AND a default fallback route pointing at it (a notifier with no route is
// unreachable: the dispatcher iterates cfg.Routes and there is no implicit
// fallback) — then fires a best-effort test alert via sendChannelTestAlertFn.
//
// It is the channel-generic counterpart to applyAlertsStep (which stays the
// Slack-only headless path for back-compat). A config-write failure ABORTS (spec
// error-handling: a half-written channel is worse than none). The test send is
// ADVISORY — a failure prints a re-test hint and returns nil, because the daemon is
// not up yet to retry and the operator is already configured.
//
// SECRET-SAFETY: the NotifierConfig flows verbatim into AddNotifierYAML (so an
// ${ENV} token survives to disk) and is NEVER printed. The success line names only
// the notifier; the underlying notifiers scrub secrets from any error they return,
// so the advisory warning is leak-safe.
func writeNotifierChannel(cmd *cobra.Command, cfgPath string, n config.NotifierConfig) error {
	if err := config.AddNotifierYAML(cfgPath, n); err != nil {
		return fmt.Errorf("write %s notifier: %w", n.Type, err)
	}
	if err := config.AddRouteYAML(cfgPath, config.RouteConfig{Notifier: n.Name}); err != nil {
		return fmt.Errorf("write %s route: %w", n.Type, err)
	}

	// cobra's Execute guarantees a non-nil context in RunE; this guard is dead in
	// production but lets direct unit-test calls (no Execute) get a usable context.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, channelTestAlertTimeout)
	defer cancel()

	if err := sendChannelTestAlertFn(ctx, n); err != nil {
		// The error is already secret-scrubbed by the notifier; safe to print. It is
		// advisory: the operator is configured, so we point at the standalone re-test
		// command rather than failing setup.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"test alert failed (you can re-test later with `rabbot notify test %s`): %v\n", n.Name, err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s alerts configured.\n", channelLabel(n.Type))
	return nil
}

// notifierDoctorLine renders the `rabbot doctor` alert-channel status (decision
// #23). With zero notifiers it returns a WARNING-tone line (not a failure verdict —
// alerts are optional) that names the gap and points at the wizard; with one or more
// it returns a neutral line reporting the count. Pure (no I/O) so it is unit-tested
// directly. It never echoes a notifier's secret — only the count.
func notifierDoctorLine(cfg *config.Config) string {
	n := 0
	if cfg != nil {
		n = len(cfg.Notifiers)
	}
	if n == 0 {
		return "  status:          no alert channel configured — changes are recorded but no one is notified (add one with `rabbot init`)"
	}
	plural := "channel"
	if n != 1 {
		plural = "channels"
	}
	return fmt.Sprintf("  status:          %d %s configured", n, plural)
}

// zeroNotifierWarning is the prominent startup line logged when no notifier is
// configured (decision #23). A monitor with zero channels has no real-time value —
// it still crawls and records every change, but no one is told — so the daemon says
// so loudly and points at the wizard. It is NON-FATAL (nothing hard-blocks); the
// pull surfaces (`rabbot report`, MCP) still work. It carries no secret.
const zeroNotifierWarning = "no alert channel configured — changes are recorded but no one is notified; " +
	"add Slack / email / a webhook with `rabbot init`, or read changes with `rabbot report`"

// startupNotifierWarning reports whether the daemon should log the zero-channel
// warning, and the message to log. Pure (no logging) so the decision is unit-tested
// directly; runDaemon emits it via the structured logger at Warn. It mirrors the
// contact-email guard: warn when the effective config has zero notifiers, stay
// silent when at least one is configured.
func startupNotifierWarning(cfg config.Config) (string, bool) {
	if len(cfg.Notifiers) == 0 {
		return zeroNotifierWarning, true
	}
	return "", false
}

// runAlertsChannelChoice drives the EXPLICIT alerts-channel step (decision #23). It
// repeatedly calls prompt for the operator's AlertChannel and, on an explicit pick
// (Slack / email / webhook / the deliberate "no alerts" acknowledgment), dispatches
// configure(ch) and finishes. It loops — never silently skipping — when:
//   - the operator aborts (Esc/Ctrl-C, huh.ErrUserAborted): re-prompt, because the
//     step is REQUIRED and a no-alerts choice is a visible option, so backing out
//     must not leave alerts in an un-chosen state (the anti-silent-skip rule), and
//   - configure fails (e.g. a transient config-write error): re-prompt so the
//     operator can retry or fall back to pull-only, rather than crashing the wizard.
//
// A genuine prompt error (not an abort) is propagated — a real TTY failure is never
// swallowed. prompt is the injectable seam (production: a huh single-select); this
// loop logic is unit-tested directly.
func runAlertsChannelChoice(prompt func() (wizard.AlertChannel, error), configure func(wizard.AlertChannel) error) error {
	for {
		ch, err := prompt()
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				// Esc at a REQUIRED step is not a skip — re-present the choice (the
				// "no alerts" option is right there to pick deliberately).
				continue
			}
			return err
		}
		if !ch.IsExplicit() {
			// Defensive: an un-resolved/zero channel is not a terminal state — re-prompt.
			continue
		}
		if cerr := configure(ch); cerr != nil {
			// A configure failure must not abort the wizard or silently skip — loop so
			// the operator can retry or choose pull-only. configure itself surfaces the
			// reason (secret-free) to the user.
			continue
		}
		return nil
	}
}

// errChannelCollectAborted is returned by configureAlertChannel when the operator
// backs out of a per-channel input form. It drives runAlertsChannelChoice to
// re-prompt (no half-written channel, no silent skip). It is secret-free.
var errChannelCollectAborted = errors.New("alert-channel setup cancelled — choose again")

// collectChannelFn is the seam that collects a concrete channel's NotifierConfig
// from the operator (production: the per-channel huh forms below). It returns the
// assembled config and ok=false if the operator aborted/left it incomplete. Tests
// replace it to drive configureAlertChannel without a TTY.
var collectChannelFn = collectChannelConfig

// configureAlertChannel realizes one explicit alerts choice (decision #23). For the
// deliberate "no alerts — pull-only" choice it writes nothing and prints the
// acknowledgment (a real terminal state, never a silent skip). For a concrete
// channel it collects the per-type fields via collectChannelFn and writes the
// notifier + route + best-effort test alert via writeNotifierChannel. An aborted or
// incomplete collection returns errChannelCollectAborted so the outer loop
// re-prompts rather than leaving a half-config.
func configureAlertChannel(cmd *cobra.Command, cfgPath string, ch wizard.AlertChannel) error {
	if ch == wizard.ChannelNone {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), wizard.PullOnlyAcknowledged)
		return nil
	}
	n, ok := collectChannelFn(cmd, ch)
	if !ok {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "(no details entered — let's choose again)")
		return errChannelCollectAborted
	}
	return writeNotifierChannel(cmd, cfgPath, n)
}

// channelLabel renders a friendly channel name for the success line (no secret).
func channelLabel(notifierType string) string {
	switch notifierType {
	case config.NotifierTypeSlack:
		return "Slack"
	case config.NotifierTypeEmail:
		return "Email"
	case config.NotifierTypeWebhook:
		return "Webhook"
	default:
		return notifierType
	}
}

// ── TTY seams (untested; exercised only by an integration `rabbot init`) ──────
//
// These collect operator input via huh forms. The pure routing/state logic above
// (runAlertsChannelChoice / configureAlertChannel / writeNotifierChannel) is what
// the unit tests drive; the huh.Form.Run calls here need a real terminal.

// runAlertsStep is the REQUIRED, EXPLICIT alerts step (decision #23): it presents
// the channel choice and loops until the operator configures a channel OR
// deliberately picks "no alerts — pull-only". It is the production driver wiring the
// TTY prompt to the unit-tested runAlertsChannelChoice loop + configureAlertChannel
// dispatch. A genuine TTY error is surfaced to stderr (advisory — the operator is
// already live); the step never silently leaves alerts un-chosen.
func runAlertsStep(cmd *cobra.Command, cfgPath string) {
	prompt := func() (wizard.AlertChannel, error) { return promptAlertChannel(cmd) }
	configure := func(ch wizard.AlertChannel) error { return configureAlertChannel(cmd, cfgPath, ch) }
	if err := runAlertsChannelChoice(prompt, configure); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "skipping the alerts step (%v)\n", err)
	}
}

// promptAlertChannel presents the channel choice (Slack / email / webhook / no
// alerts) as a single-select. It returns the resolved AlertChannel; an Esc/Ctrl-C
// abort surfaces huh.ErrUserAborted so runAlertsChannelChoice re-prompts (the
// required step has no silent skip — "no alerts" is a visible option to pick).
func promptAlertChannel(cmd *cobra.Command) (wizard.AlertChannel, error) {
	opts := wizard.AlertChannelOptions()
	huhOpts := make([]huh.Option[string], 0, len(opts))
	for _, o := range opts {
		huhOpts = append(huhOpts, huh.NewOption(o.Label, o.Value))
	}
	var value string
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("How do you want to be alerted when your site changes?").
			Description("Pick one. You can configure more later by re-running `rabbot init`.").
			Options(huhOpts...).
			Value(&value),
	)).WithInput(cmd.InOrStdin()).WithOutput(cmd.OutOrStdout())
	if err := form.Run(); err != nil {
		return wizard.ChannelUnset, err
	}
	return wizard.ResolveAlertChannel(value)
}

// collectChannelConfig dispatches to the per-channel collector. It is the production
// collectChannelFn. ok=false means the operator backed out / left it incomplete.
func collectChannelConfig(cmd *cobra.Command, ch wizard.AlertChannel) (config.NotifierConfig, bool) {
	switch ch {
	case wizard.ChannelSlack:
		webhook := promptSlackWebhook(cmd)
		if webhook == "" {
			return config.NotifierConfig{}, false
		}
		return config.NotifierConfig{Name: "slack", Type: config.NotifierTypeSlack, URL: webhook}, true
	case wizard.ChannelEmail:
		return promptEmailNotifier(cmd)
	case wizard.ChannelWebhook:
		return promptWebhookNotifier(cmd)
	default:
		return config.NotifierConfig{}, false
	}
}

// promptEmailNotifier collects the email-smtp fields. The password is collected with
// a MASKED input (never echoed). A missing host/from/to (the daemon-required fields)
// returns ok=false so the step re-prompts rather than writing an unloadable notifier.
func promptEmailNotifier(cmd *cobra.Command) (config.NotifierConfig, bool) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, wizard.EmailWalkthrough)

	var host, portStr, username, password, from, toStr string
	portStr = "587"
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title(wizard.EmailHostPrompt).Value(&host),
		huh.NewInput().Title(wizard.EmailPortPrompt).Value(&portStr),
		huh.NewInput().Title(wizard.EmailUsernamePrompt).Value(&username),
		huh.NewInput().Title(wizard.EmailPasswordPrompt).EchoMode(huh.EchoModePassword).Value(&password),
		huh.NewInput().Title(wizard.EmailFromPrompt).Value(&from),
		huh.NewInput().Title(wizard.EmailToPrompt).Value(&toStr),
	)).WithInput(cmd.InOrStdin()).WithOutput(out)
	if err := form.Run(); err != nil {
		return config.NotifierConfig{}, false
	}

	n := config.NotifierConfig{
		Name:     "email",
		Type:     config.NotifierTypeEmail,
		SMTPHost: strings.TrimSpace(host),
		SMTPPort: parsePortOrDefault(portStr, 587),
		Username: strings.TrimSpace(username),
		Password: password, // verbatim: may be an ${ENV} token; never trimmed-away or printed
		From:     strings.TrimSpace(from),
		To:       splitAddresses(toStr),
	}
	// Validate the required-field contract up front so we re-prompt instead of
	// writing a notifier the daemon would reject. ValidateNotifiers names the missing
	// field without echoing any secret.
	if verr := config.ValidateNotifiers([]config.NotifierConfig{n}); verr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "email setup incomplete: %v\n", verr)
		return config.NotifierConfig{}, false
	}
	return n, true
}

// promptWebhookNotifier collects the generic-webhook url + an OPTIONAL Authorization
// header (masked). A missing url returns ok=false to re-prompt.
func promptWebhookNotifier(cmd *cobra.Command) (config.NotifierConfig, bool) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, wizard.WebhookWalkthrough)

	var url, auth string
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title(wizard.WebhookURLPrompt).Value(&url),
		huh.NewInput().Title(wizard.WebhookAuthPrompt).EchoMode(huh.EchoModePassword).Value(&auth),
	)).WithInput(cmd.InOrStdin()).WithOutput(out)
	if err := form.Run(); err != nil {
		return config.NotifierConfig{}, false
	}

	n := config.NotifierConfig{
		Name: "webhook",
		Type: config.NotifierTypeWebhook,
		URL:  strings.TrimSpace(url),
	}
	if a := strings.TrimSpace(auth); a != "" {
		// Only the VALUE may be a secret/${ENV} token; the header key is fixed.
		// Store the trimmed value — under EchoModePassword the operator can't see
		// stray leading/trailing whitespace, which would otherwise propagate into
		// every outgoing Authorization header.
		n.Headers = map[string]string{"Authorization": a}
	}
	if verr := config.ValidateNotifiers([]config.NotifierConfig{n}); verr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "webhook setup incomplete: %v\n", verr)
		return config.NotifierConfig{}, false
	}
	return n, true
}

// parsePortOrDefault parses a TCP port string, falling back to def on a bad/empty
// value (the operator can fix it later; ValidateNotifiers still guards the result).
func parsePortOrDefault(s string, def int) int {
	p, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || p <= 0 || p > 65535 {
		return def
	}
	return p
}

// splitAddresses splits a comma-separated recipient list into trimmed, non-empty
// addresses (the email "To" field accepts one or more).
func splitAddresses(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if a := strings.TrimSpace(part); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// sendChannelTestAlert builds a notifier of the configured type and sends the
// canonical synthetic alert inline (no daemon). It mirrors notify.SendTestAlert but
// for any channel. The returned error is whatever the notifier returns — already
// scrubbed of secrets by the email/webhook/slack backends.
func sendChannelTestAlert(ctx context.Context, n config.NotifierConfig) error {
	client := &http.Client{Timeout: channelTestAlertTimeout}
	var (
		notifier notify.Notifier
		err      error
	)
	switch n.Type {
	case config.NotifierTypeSlack:
		notifier = notify.NewSlackNotifier(n.Name, n.URL, client)
	case config.NotifierTypeWebhook:
		notifier, err = notify.NewWebhookNotifier(n.Name, n.URL, n.Headers, client)
	case config.NotifierTypeEmail:
		notifier, err = notify.NewEmailNotifier(notify.EmailConfig{
			Name:           n.Name,
			Host:           n.SMTPHost,
			Port:           n.SMTPPort,
			Username:       n.Username,
			Password:       n.Password,
			From:           n.From,
			To:             n.To,
			AllowPlaintext: n.AllowPlaintext,
		})
	default:
		// Unknown type: nothing to send (validation elsewhere surfaces the misconfig).
		return fmt.Errorf("notify: unknown notifier type %q", n.Type)
	}
	if err != nil {
		return err
	}
	return notifier.Notify(ctx, notify.SampleTestAlert())
}
