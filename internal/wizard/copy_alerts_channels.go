package wizard

// Onboarding copy for the email and generic-webhook alert channels (decision #23:
// email is one option among three, no special treatment). It mirrors copy_alerts.go
// (the Slack walkthrough): FIXED, jargon-light text shown BEFORE the operator pastes
// anything, so it never embeds a live secret — passwords / auth-header values are
// collected via a masked input and never printed. The runner renders these on a TTY
// (huh); they are stated plainly here, in one place.

// EmailWalkthrough is the plain walkthrough shown at the top of the email-SMTP
// alerts step. It names what Rabbot needs (an SMTP relay) without assuming a
// provider, and is honest that the password is a secret kept private.
const EmailWalkthrough = "To get email alerts, Rabbot sends a short message through your mail " +
	"provider's SMTP server whenever your site changes.\n\n" +
	"You'll need a few details from your email provider (or your own mail server):\n" +
	"  1. The SMTP host and port (port 465 uses TLS directly; other ports upgrade with STARTTLS).\n" +
	"  2. A username and password, if your provider requires sign-in (most do).\n" +
	"  3. The From address to send as, and one or more recipients.\n\n" +
	"The password stays hidden as you type, and we never print it back."

const (
	// EmailHostPrompt..EmailToPrompt are the per-field input titles for the email
	// step. EmailPasswordPrompt explicitly promises masking (it is collected with a
	// hidden input). EmailToPrompt accepts a comma-separated list.
	EmailHostPrompt     = "SMTP host (e.g. smtp.your-provider.com)"
	EmailPortPrompt     = "SMTP port (465 for direct TLS, 587 for STARTTLS, 25 for a local relay)"
	EmailUsernamePrompt = "SMTP username (leave blank for an unauthenticated local relay)"
	// #nosec G101 -- this is a UI PROMPT LABEL shown to the operator, not a credential value.
	EmailPasswordPrompt = "SMTP password (stays hidden as you type; leave blank if your relay needs no sign-in)"
	EmailFromPrompt     = "From address (who the alert is sent as, e.g. rabbot@your-domain.com)"
	EmailToPrompt       = "Send alerts to (one or more addresses, comma-separated)"
)

// WebhookWalkthrough is the plain walkthrough shown at the top of the
// generic-webhook alerts step. It frames the webhook as the channel that unlocks
// everything else (PagerDuty, ntfy, automation platforms, self-hosted glue) and is
// honest that an optional auth header value is a secret.
const WebhookWalkthrough = "A webhook lets Rabbot POST each alert as JSON to a URL you control — the " +
	"channel that connects everything else: PagerDuty, ntfy, automation platforms " +
	"(n8n / Zapier / Make), or your own service.\n\n" +
	"You'll need:\n" +
	"  1. The URL to POST alerts to.\n" +
	"  2. (Optional) an Authorization header value, if the endpoint requires auth.\n\n" +
	"Any auth value stays hidden as you type, and we never print it back."

const (
	// WebhookURLPrompt collects the POST target. WebhookAuthPrompt collects an
	// OPTIONAL Authorization header value with a masked input (it explicitly
	// promises masking).
	WebhookURLPrompt  = "Webhook URL to POST alerts to (e.g. https://example.com/hooks/rabbot)"
	WebhookAuthPrompt = "Authorization header value (optional; stays hidden as you type — e.g. \"Bearer abc123\")"
)

// PullOnlyAcknowledged is the confirmation line shown when the operator explicitly
// chooses "no alerts — CLI/MCP only". It states plainly that monitoring still runs
// and how to add a channel later, so the deliberate choice never reads as a silent
// skip. It carries no secret, so it is safe to print.
const PullOnlyAcknowledged = "No alert channel configured — monitoring runs in CLI/MCP-only mode. " +
	"Rabbot still crawls and records every change; you'll read them with `rabbot report`, " +
	"`rabbot history <url>`, or your MCP host. Add a channel any time by re-running `rabbot init`."
