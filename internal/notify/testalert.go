package notify

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// ErrNoWebhook is the sentinel SendTestAlert returns when no webhook is
// configured, so callers can treat "nothing configured" distinctly from a send
// failure (the onboarding caller skips the warning in that case).
var ErrNoWebhook = errors.New("notify: no webhook configured")

// SendTestAlert delivers a single synthetic alert directly to a Slack Incoming
// Webhook, without going through the daemon's control plane.
//
// RATIONALE: at ONBOARDING the daemon is NOT running (it starts in step 10), so
// the wizard/headless test-alert CANNOT route through control.NotifyTest (which
// requires the daemon up). SendTestAlert builds the notifier directly from the
// webhook URL via the EXISTING NewSlackNotifier and sends the sample alert inline
// in the init process. It is best-effort: callers treat any error as a non-fatal
// warning. The slackNotifier already SCRUBS the webhook URL from any returned
// error (slack.go), so a surfaced error is safe to print (CLAUDE.md: Slack
// webhook URLs must never be logged). An empty webhookURL returns ErrNoWebhook.
//
// A nil client is fine: NewSlackNotifier defaults it to a 30s-timeout client.
func SendTestAlert(ctx context.Context, webhookURL string, client *http.Client) error {
	if webhookURL == "" {
		return ErrNoWebhook
	}
	n := NewSlackNotifier("slack-test", webhookURL, client)
	return n.Notify(ctx, sampleTestAlert())
}

// SampleTestAlert returns the canonical synthetic alert used by every test-alert
// surface (onboarding Slack send, `rabbot notify test`, and the onboarding
// email/generic-webhook channels). Exposing it lets the cli onboarding step build a
// notifier of any type from config and send the SAME sample inline (the daemon is
// not up yet at onboarding), so a test alert is indistinguishable across channels
// and across the onboarding/runtime paths.
func SampleTestAlert() Alert { return sampleTestAlert() }

// sampleTestAlert mirrors the daemon's existing `notify test` alert
// (internal/cli/run.go) so an onboarding test-alert is indistinguishable from
// one triggered later via `rabbot notify test`.
func sampleTestAlert() Alert {
	return Alert{
		Site:       "rabbot-test.example",
		URL:        "https://rabbot-test.example/",
		ChangeType: "notify_test",
		Severity:   model.SeverityInfo,
		Before:     "(test before)",
		After:      "(test after)",
		DetectedAt: time.Now().UTC(),
		DeepLink:   "https://rabbot-test.example/",
	}
}
