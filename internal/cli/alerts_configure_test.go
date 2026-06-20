package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/wizard"
)

// TestConfigureAlertChannel_NoneAcknowledges pins that the explicit "no alerts —
// pull-only" choice writes NOTHING but prints the deliberate acknowledgment (so it
// reads as a real choice, never a silent skip). Returns nil (a terminal state).
func TestConfigureAlertChannel_NoneAcknowledges(t *testing.T) {
	cfgPath := seedChannelCfg(t)

	var out bytes.Buffer
	cmd := persistCmd()
	cmd.SetOut(&out)

	if err := configureAlertChannel(cmd, cfgPath, wizard.ChannelNone); err != nil {
		t.Fatalf("ChannelNone must be a clean terminal state: %v", err)
	}
	cfg, _ := config.Load(cfgPath, nil)
	if len(cfg.Notifiers) != 0 {
		t.Fatalf("pull-only choice must write no notifier, got: %+v", cfg.Notifiers)
	}
	if !strings.Contains(out.String(), "CLI/MCP-only") {
		t.Fatalf("pull-only choice must print the acknowledgment, got: %q", out.String())
	}
}

// TestConfigureAlertChannel_CollectedConfigIsWritten pins that for a concrete
// channel, the collected NotifierConfig (via the collectChannelFn seam) is written.
// A successful collection => the notifier lands in config.
func TestConfigureAlertChannel_CollectedConfigIsWritten(t *testing.T) {
	cfgPath := seedChannelCfg(t)

	prevCollect := collectChannelFn
	collectChannelFn = func(_ *cobra.Command, ch wizard.AlertChannel) (config.NotifierConfig, bool) {
		// Simulate the operator completing the email form.
		return config.NotifierConfig{
			Name: "email", Type: ch.NotifierType(),
			SMTPHost: "smtp.example.com", SMTPPort: 465,
			From: "a@b.com", To: []string{"c@d.com"},
		}, true
	}
	t.Cleanup(func() { collectChannelFn = prevCollect })

	prevSend := sendChannelTestAlertFn
	sendChannelTestAlertFn = func(context.Context, config.NotifierConfig) error { return nil }
	t.Cleanup(func() { sendChannelTestAlertFn = prevSend })

	if err := configureAlertChannel(persistCmd(), cfgPath, wizard.ChannelEmail); err != nil {
		t.Fatalf("configureAlertChannel(email): %v", err)
	}
	cfg, _ := config.Load(cfgPath, nil)
	if len(cfg.Notifiers) != 1 || cfg.Notifiers[0].Type != config.NotifierTypeEmail {
		t.Fatalf("collected email notifier not written: %+v", cfg.Notifiers)
	}
}

// TestConfigureAlertChannel_AbortedCollectionReprompts pins that if the operator
// backs out of the per-channel form (collectChannelFn returns ok=false), the step
// does NOT write a half-config and returns an error so the outer loop re-prompts
// (no silent skip). Nothing is written.
func TestConfigureAlertChannel_AbortedCollectionReprompts(t *testing.T) {
	cfgPath := seedChannelCfg(t)

	prevCollect := collectChannelFn
	collectChannelFn = func(*cobra.Command, wizard.AlertChannel) (config.NotifierConfig, bool) {
		return config.NotifierConfig{}, false // operator backed out
	}
	t.Cleanup(func() { collectChannelFn = prevCollect })

	err := configureAlertChannel(persistCmd(), cfgPath, wizard.ChannelWebhook)
	if err == nil {
		t.Fatal("an aborted collection must return an error so the loop re-prompts (no silent skip)")
	}
	cfg, _ := config.Load(cfgPath, nil)
	if len(cfg.Notifiers) != 0 {
		t.Fatalf("an aborted collection must write nothing, got: %+v", cfg.Notifiers)
	}
}
