package wizard

// SlackDocsURL is the deep-link to Slack's OWN Incoming-Webhooks documentation,
// shown in the guided alerts step as a "show me how" pointer. We link Slack's docs
// rather than reproduce their UI (which goes stale). It is a public docs page — no
// secret — so it is always safe to print.
const SlackDocsURL = "https://api.slack.com/messaging/webhooks"

// SlackWalkthrough is the plain, jargon-light walkthrough shown at the top of the
// guided Slack alerts step (the post-go-live "get notified when it changes"
// upgrade). It names the Slack "Incoming Webhook" the operator must create and
// describes the steps in order, then defers to Slack's own docs (SlackDocsURL) for
// the exact clicks. It is FIXED copy shown BEFORE the operator pastes anything, so
// it never embeds a live webhook URL — that value is a secret collected via a
// masked input and never printed.
const SlackWalkthrough = "To get a Slack alert when your site changes, Rabbot needs an " +
	"Incoming Webhook — a private link Slack gives you that posts messages into one channel.\n\n" +
	"Here's the gist:\n" +
	"  1. Open Slack's app settings and create (or reuse) an app for your workspace.\n" +
	"  2. Turn on \"Incoming Webhooks\" and add one to the channel you want alerts in.\n" +
	"  3. Copy the webhook URL Slack gives you — it's a secret, so keep it private.\n\n" +
	"We'll link Slack's own step-by-step guide next, then ask you to paste the URL " +
	"(it stays hidden as you type, and we never print it back)."

// SlackDocsHint is the one-line "show me how" pointer rendered just above the
// masked webhook input, pairing the friendly label with the public docs URL. It is
// safe to print (SlackDocsURL carries no secret).
const SlackDocsHint = "Show me how (opens Slack's docs): " + SlackDocsURL

// SlackWebhookPrompt is the masked-input title shown when collecting the webhook,
// and SlackArrivedTitle is the post-send "did it arrive?" confirm title. The runner
// renders these on a TTY (huh) so they are stated plainly here, in one place.
const (
	SlackWebhookPrompt = "Paste your Slack Incoming-Webhook URL (it stays hidden as you type)"
	SlackArrivedTitle  = "Did a test message just arrive in your Slack channel?"
)

// SlackNotArrivedTip is the advisory shown when the operator says the test alert did
// NOT arrive: it points at the standalone re-test command so they never have to
// re-run setup. It carries no secret (no webhook URL), so it is safe to print.
func SlackNotArrivedTip() string {
	return "No test message yet? Double-check the webhook's channel, then re-test any " +
		"time with `rabbot notify test slack` — no need to re-run setup."
}
