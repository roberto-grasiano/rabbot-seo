package cli

import (
	"errors"
	"testing"

	"github.com/charmbracelet/huh"

	"github.com/roberto-grasiano/rabbot-seo/internal/wizard"
)

// TestRunAlertsChannelChoice_ConfiguresOnePerExplicitPick pins the core of
// decision #23: the loop ends as soon as the operator makes an EXPLICIT pick, and
// it dispatches exactly that channel's configuration once.
func TestRunAlertsChannelChoice_ConfiguresOnePerExplicitPick(t *testing.T) {
	for _, ch := range []wizard.AlertChannel{
		wizard.ChannelSlack, wizard.ChannelEmail, wizard.ChannelWebhook, wizard.ChannelNone,
	} {
		var configured []wizard.AlertChannel
		prompt := func() (wizard.AlertChannel, error) { return ch, nil }
		err := runAlertsChannelChoice(prompt, func(got wizard.AlertChannel) error {
			configured = append(configured, got)
			return nil
		})
		if err != nil {
			t.Fatalf("channel %v: unexpected error: %v", ch, err)
		}
		if len(configured) != 1 || configured[0] != ch {
			t.Fatalf("channel %v: want exactly one dispatch of %v, got %v", ch, ch, configured)
		}
	}
}

// TestRunAlertsChannelChoice_NoSilentSkipOnAbort is the anti-silent-skip guard: an
// Esc/Ctrl-C abort at the choice prompt must NOT end the step in a no-channel state
// — it re-prompts. The operator must make a positive choice (configure a channel OR
// explicitly pick "no alerts"). The fake prompt aborts once, then returns an
// explicit choice; the loop must re-prompt and only finish on the explicit choice.
func TestRunAlertsChannelChoice_NoSilentSkipOnAbort(t *testing.T) {
	calls := 0
	prompt := func() (wizard.AlertChannel, error) {
		calls++
		if calls == 1 {
			return wizard.ChannelUnset, huh.ErrUserAborted // first: user hits Esc
		}
		return wizard.ChannelNone, nil // second: explicit pull-only choice
	}
	var configured []wizard.AlertChannel
	err := runAlertsChannelChoice(prompt, func(got wizard.AlertChannel) error {
		configured = append(configured, got)
		return nil
	})
	if err != nil {
		t.Fatalf("abort-then-choose must succeed, got: %v", err)
	}
	if calls != 2 {
		t.Fatalf("an abort must RE-PROMPT (no silent skip), want 2 prompt calls, got %d", calls)
	}
	if len(configured) != 1 || configured[0] != wizard.ChannelNone {
		t.Fatalf("want one dispatch of ChannelNone after re-prompt, got %v", configured)
	}
}

// TestRunAlertsChannelChoice_ConfigureErrorReprompts pins that a failed
// configuration (e.g. a transient config-write error) does NOT crash the step and
// does NOT leave it silently skipped — it re-prompts so the operator can retry or
// choose pull-only. A real (non-abort) prompt error IS propagated.
func TestRunAlertsChannelChoice_ConfigureErrorReprompts(t *testing.T) {
	calls := 0
	prompt := func() (wizard.AlertChannel, error) {
		calls++
		if calls == 1 {
			return wizard.ChannelEmail, nil
		}
		return wizard.ChannelNone, nil
	}
	dispatch := func(ch wizard.AlertChannel) error {
		if ch == wizard.ChannelEmail {
			return errors.New("write failed")
		}
		return nil
	}
	if err := runAlertsChannelChoice(prompt, dispatch); err != nil {
		t.Fatalf("a configure error should re-prompt, not abort the wizard: %v", err)
	}
	if calls != 2 {
		t.Fatalf("a configure failure must re-prompt, want 2 prompt calls, got %d", calls)
	}
}

// TestRunAlertsChannelChoice_PropagatesRealPromptError pins that a genuine TTY
// failure (not an abort) is surfaced, never swallowed.
func TestRunAlertsChannelChoice_PropagatesRealPromptError(t *testing.T) {
	wantErr := errors.New("tty exploded")
	prompt := func() (wizard.AlertChannel, error) { return wizard.ChannelUnset, wantErr }
	err := runAlertsChannelChoice(prompt, func(wizard.AlertChannel) error { return nil })
	if !errors.Is(err, wantErr) {
		t.Fatalf("real prompt error must propagate, got: %v", err)
	}
}
