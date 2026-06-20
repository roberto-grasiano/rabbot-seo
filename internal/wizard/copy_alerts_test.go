package wizard

import (
	"strings"
	"testing"
)

func TestSlackWalkthrough_MentionsIncomingWebhook(t *testing.T) {
	if !strings.Contains(SlackWalkthrough, "Incoming Webhook") {
		t.Fatalf("walkthrough should name the Slack Incoming Webhook, got %q", SlackWalkthrough)
	}
}

func TestSlackWalkthrough_NoWebhookSecretLeak(t *testing.T) {
	// The walkthrough is fixed copy shown BEFORE the operator pastes anything, so it
	// must not embed a real hooks.slack.com webhook URL (those are secrets). It may
	// name the docs page, but never a live webhook.
	if strings.Contains(SlackWalkthrough, "hooks.slack.com") {
		t.Fatalf("walkthrough %q must not embed a live webhook URL", SlackWalkthrough)
	}
}

func TestSlackDocsURL_PointsAtWebhooksDoc(t *testing.T) {
	// The "show me how" deep-link points at Slack's own Incoming-Webhooks doc, not a
	// live webhook endpoint.
	if !strings.Contains(SlackDocsURL, "api.slack.com/messaging/webhooks") {
		t.Fatalf("docs URL %q should point at Slack's incoming-webhooks doc", SlackDocsURL)
	}
}

func TestSlackNotArrivedTip_NamesTheNotifier(t *testing.T) {
	// The re-test command MUST name the notifier: `rabbot notify test` takes
	// cobra.ExactArgs(1), so a bare `rabbot notify test` exits non-zero. The tip
	// the operator copy-pastes has to be the runnable `rabbot notify test slack`.
	tip := SlackNotArrivedTip()
	if !strings.Contains(tip, "notify test slack") {
		t.Fatalf("tip should point at the runnable `rabbot notify test slack`, got %q", tip)
	}
}
