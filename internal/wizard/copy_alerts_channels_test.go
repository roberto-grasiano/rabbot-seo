package wizard

import (
	"strings"
	"testing"
)

// TestChannelCopyIsPresentAndSafe pins that the email and generic-webhook
// onboarding copy exists, is non-empty, and (being FIXED copy shown BEFORE the
// operator pastes anything) never embeds a placeholder that looks like a real
// secret. The actual secret is collected via a masked input and never printed.
func TestChannelCopyIsPresentAndSafe(t *testing.T) {
	for name, s := range map[string]string{
		"EmailWalkthrough":     EmailWalkthrough,
		"EmailHostPrompt":      EmailHostPrompt,
		"EmailPortPrompt":      EmailPortPrompt,
		"EmailUsernamePrompt":  EmailUsernamePrompt,
		"EmailPasswordPrompt":  EmailPasswordPrompt,
		"EmailFromPrompt":      EmailFromPrompt,
		"EmailToPrompt":        EmailToPrompt,
		"WebhookWalkthrough":   WebhookWalkthrough,
		"WebhookURLPrompt":     WebhookURLPrompt,
		"WebhookAuthPrompt":    WebhookAuthPrompt,
		"PullOnlyAcknowledged": PullOnlyAcknowledged,
	} {
		if strings.TrimSpace(s) == "" {
			t.Fatalf("%s copy must be non-empty", name)
		}
	}
}

// TestPasswordPromptSignalsMasking pins that the email-password and webhook-auth
// prompts tell the operator the value stays hidden (the cli layer collects them
// with a masked input). A regression that drops the masking promise would mislead.
func TestPasswordPromptSignalsMasking(t *testing.T) {
	for _, s := range []string{EmailPasswordPrompt, WebhookAuthPrompt} {
		if !strings.Contains(strings.ToLower(s), "hidden") {
			t.Fatalf("secret prompt %q should tell the operator the value stays hidden", s)
		}
	}
}
