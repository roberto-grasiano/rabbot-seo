package wizard

import "testing"

// TestResolveAlertChannel_RoundTrips asserts every offered alert-channel option
// value resolves back to a distinct AlertChannel, and that an unknown value is an
// error (so a drifted huh option can never silently map to the wrong channel).
// This mirrors the ResolveExistingAction idiom.
func TestResolveAlertChannel_RoundTrips(t *testing.T) {
	opts := AlertChannelOptions()
	if len(opts) < 4 {
		t.Fatalf("want at least 4 channel options (slack, email, webhook, none), got %d", len(opts))
	}
	seen := make(map[AlertChannel]bool, len(opts))
	for _, o := range opts {
		if o.Label == "" {
			t.Fatalf("option value %q has an empty label", o.Value)
		}
		ch, err := ResolveAlertChannel(o.Value)
		if err != nil {
			t.Fatalf("offered option value %q did not resolve: %v", o.Value, err)
		}
		if seen[ch] {
			t.Fatalf("option value %q maps to an already-seen channel %v", o.Value, ch)
		}
		seen[ch] = true
	}
	// All four channels must be reachable from the offered options.
	for _, want := range []AlertChannel{ChannelSlack, ChannelEmail, ChannelWebhook, ChannelNone} {
		if !seen[want] {
			t.Fatalf("channel %v is not reachable from AlertChannelOptions()", want)
		}
	}
}

// TestResolveAlertChannel_UnknownIsError pins that an unknown option string is a
// hard error, never a silent default to some channel.
func TestResolveAlertChannel_UnknownIsError(t *testing.T) {
	if ch, err := ResolveAlertChannel("garbage"); err == nil {
		t.Fatalf("ResolveAlertChannel(garbage) = %v, want error", ch)
	}
}

// TestAlertChannelExplicitState is the heart of decision #23: the alerts step must
// end in an EXPLICIT state. Every concrete channel — INCLUDING the deliberate
// "no alerts / pull-only" acknowledgment — is explicit; only the zero value (an
// un-chosen step) is non-explicit. The cli layer re-prompts until IsExplicit, so a
// silent skip is impossible.
func TestAlertChannelExplicitState(t *testing.T) {
	for _, ch := range []AlertChannel{ChannelSlack, ChannelEmail, ChannelWebhook, ChannelNone} {
		if !ch.IsExplicit() {
			t.Fatalf("channel %v must count as an explicit choice", ch)
		}
	}
	if ChannelUnset.IsExplicit() {
		t.Fatal("the zero/unset channel must NOT count as explicit (forces a deliberate pick)")
	}
}

// TestChannelNoneConfiguresNothing pins that the pull-only acknowledgment is a
// real, distinct terminal state that writes no notifier — it is NOT a silent skip
// but an explicit "CLI/MCP only" mode the operator actively selected.
func TestChannelNoneConfiguresNothing(t *testing.T) {
	if ChannelNone == ChannelUnset {
		t.Fatal("ChannelNone (explicit pull-only) must differ from ChannelUnset (no choice made)")
	}
	if !ChannelNone.IsExplicit() {
		t.Fatal("pull-only acknowledgment must be an explicit terminal state")
	}
	// A notifier type string is the public contract; the no-alerts choice has none.
	if got := ChannelNone.NotifierType(); got != "" {
		t.Fatalf("ChannelNone.NotifierType() = %q, want empty (writes no notifier)", got)
	}
}

// TestChannelNotifierTypeContract pins the EXACT public type strings (decision #10):
// "slack-webhook", "email-smtp", "generic-webhook". These are a public contract; a
// typo here would write a config the daemon's BuildAlertingStack switch rejects.
func TestChannelNotifierTypeContract(t *testing.T) {
	cases := map[AlertChannel]string{
		ChannelSlack:   "slack-webhook",
		ChannelEmail:   "email-smtp",
		ChannelWebhook: "generic-webhook",
	}
	for ch, want := range cases {
		if got := ch.NotifierType(); got != want {
			t.Fatalf("channel %v NotifierType() = %q, want %q", ch, got, want)
		}
	}
}
