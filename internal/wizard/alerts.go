package wizard

import "fmt"

// AlertChannel is the operator's choice at the onboarding alerts step. Decision
// #23 (2026-06-10) makes this an EXPLICIT pick: the step ends either configuring
// one channel (Slack / email / generic webhook) or actively acknowledging
// "no alerts — pull-only (CLI/MCP) mode". Email is one option among three, with no
// special treatment. It is a pure enum so the routing/state decision is
// unit-testable independent of the huh.Select that collects it (mirrors
// ExistingAction / Upgrade).
type AlertChannel int

const (
	// ChannelUnset is the zero value: no choice has been made yet. It is NOT an
	// explicit terminal state — the cli alerts loop re-prompts until the operator
	// picks a concrete option, so a silent skip is impossible (decision #23).
	ChannelUnset AlertChannel = iota
	// ChannelSlack configures a Slack Incoming-Webhook notifier ("slack-webhook").
	ChannelSlack
	// ChannelEmail configures an SMTP email notifier ("email-smtp").
	ChannelEmail
	// ChannelWebhook configures a generic JSON webhook notifier ("generic-webhook").
	ChannelWebhook
	// ChannelNone is the deliberate "no alerts — CLI/MCP only" acknowledgment. It is
	// an EXPLICIT terminal state the operator actively selects (not a silent skip);
	// it writes no notifier. A monitor with zero channels has no real-time value, so
	// the summary/run/doctor surfaces still flag the zero-channel state — but nothing
	// hard-blocks (decision #23).
	ChannelNone
)

// alertChannelValues maps the huh option string carried by the Select to a channel.
// It is the SINGLE source of truth shared by AlertChannelOptions (the menu) and
// ResolveAlertChannel (the reverse mapping), so the screen and the parser can never
// drift out of sync.
var alertChannelValues = map[string]AlertChannel{
	"slack":   ChannelSlack,
	"email":   ChannelEmail,
	"webhook": ChannelWebhook,
	"none":    ChannelNone,
}

// AlertChannelOption is one selectable row of the alerts step: a friendly Label and
// the stable Value the huh.Select carries (and that ResolveAlertChannel maps back).
type AlertChannelOption struct {
	Label string
	Value string
}

// alertChannelOrder fixes the menu order (Slack first — the most common pick — then
// email, generic webhook, and finally the explicit pull-only acknowledgment), so
// AlertChannelOptions is deterministic rather than ranging a map.
var alertChannelOrder = []AlertChannelOption{
	{Label: "Slack — get alerts in a channel", Value: "slack"},
	{Label: "Email — get alerts by email (SMTP)", Value: "email"},
	{Label: "Webhook — POST alerts to a URL (PagerDuty, ntfy, n8n/Zapier, your own glue)", Value: "webhook"},
	{Label: "No alerts for now — monitor in CLI/MCP-only mode", Value: "none"},
}

// AlertChannelOptions returns the alert-channel options in menu order. The cli
// huh.Select builds its options from this list and feeds the chosen Value back to
// ResolveAlertChannel, so the two stay in lockstep.
func AlertChannelOptions() []AlertChannelOption {
	out := make([]AlertChannelOption, len(alertChannelOrder))
	copy(out, alertChannelOrder)
	return out
}

// ResolveAlertChannel maps a choice string (the value carried by the huh.Select
// option) to an AlertChannel, returning an error for an unknown choice. Keeping
// this pure lets the cli layer drive the production huh.Select while tests assert
// the mapping directly.
func ResolveAlertChannel(s string) (AlertChannel, error) {
	if ch, ok := alertChannelValues[s]; ok {
		return ch, nil
	}
	return ChannelUnset, fmt.Errorf("wizard: unknown alert-channel choice %q", s)
}

// IsExplicit reports whether this channel represents a deliberate choice that ends
// the alerts step. Every concrete option — INCLUDING the pull-only acknowledgment
// (ChannelNone) — is explicit; only ChannelUnset is not, which forces the cli loop
// to re-prompt (decision #23: no silent skip).
func (c AlertChannel) IsExplicit() bool {
	switch c {
	case ChannelSlack, ChannelEmail, ChannelWebhook, ChannelNone:
		return true
	default:
		return false
	}
}

// NotifierType returns the public notifier type string this channel writes to
// config — the settled public contract (decision #10): "slack-webhook",
// "email-smtp", "generic-webhook". ChannelNone (and ChannelUnset) configure no
// notifier and return "".
func (c AlertChannel) NotifierType() string {
	switch c {
	case ChannelSlack:
		return "slack-webhook"
	case ChannelEmail:
		return "email-smtp"
	case ChannelWebhook:
		return "generic-webhook"
	default:
		return ""
	}
}
