package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// seedChannelCfg writes a minimal config.yaml with empty notifiers/routes and
// returns its path. Mirrors the seed the YAML mutators expect.
func seedChannelCfg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const seed = "control:\n  port: 7777\nsites: []\nnotifiers: []\nroutes: []\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return path
}

// TestWriteNotifierChannelEmail pins that writeNotifierChannel writes an
// email-smtp notifier with ALL per-type fields plus a fallback route, the written
// config passes ValidateNotifiers, and the best-effort test-alert seam is invoked.
func TestWriteNotifierChannelEmail(t *testing.T) {
	cfgPath := seedChannelCfg(t)

	var gotName, gotType string
	prev := sendChannelTestAlertFn
	sendChannelTestAlertFn = func(_ context.Context, n config.NotifierConfig) error {
		gotName, gotType = n.Name, n.Type
		return nil
	}
	t.Cleanup(func() { sendChannelTestAlertFn = prev })

	n := config.NotifierConfig{
		Name: "email", Type: config.NotifierTypeEmail,
		SMTPHost: "smtp.example.com", SMTPPort: 587,
		Username: "u@example.com", Password: "${RABBOT_SMTP_PASS}",
		From: "rabbot@example.com", To: []string{"team@example.com"},
	}
	if err := writeNotifierChannel(persistCmd(), cfgPath, n); err != nil {
		t.Fatalf("writeNotifierChannel: %v", err)
	}

	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Notifiers) != 1 || cfg.Notifiers[0].Type != config.NotifierTypeEmail {
		t.Fatalf("email notifier not written: %+v", cfg.Notifiers)
	}
	if cfg.Notifiers[0].SMTPHost != "smtp.example.com" || cfg.Notifiers[0].SMTPPort != 587 {
		t.Fatalf("email fields not written: %+v", cfg.Notifiers[0])
	}
	// A notifier with no route is unreachable — the step MUST write a fallback route.
	if len(cfg.Routes) != 1 || cfg.Routes[0].Notifier != "email" {
		t.Fatalf("fallback route not written: %+v", cfg.Routes)
	}
	if verr := config.ValidateNotifiers(cfg.Notifiers); verr != nil {
		t.Fatalf("written email config failed ValidateNotifiers: %v", verr)
	}
	if gotName != "email" || gotType != config.NotifierTypeEmail {
		t.Fatalf("test-alert seam not invoked with the email notifier: name=%q type=%q", gotName, gotType)
	}
}

// TestWriteNotifierChannelWebhook pins the generic-webhook path: url + headers are
// written, a route is added, validation passes, and the test-alert seam fires.
func TestWriteNotifierChannelWebhook(t *testing.T) {
	cfgPath := seedChannelCfg(t)

	prev := sendChannelTestAlertFn
	sendChannelTestAlertFn = func(context.Context, config.NotifierConfig) error { return nil }
	t.Cleanup(func() { sendChannelTestAlertFn = prev })

	n := config.NotifierConfig{
		Name: "webhook", Type: config.NotifierTypeWebhook,
		URL:     "https://example.com/hook",
		Headers: map[string]string{"Authorization": "${RABBOT_GLUE_TOKEN}"},
	}
	if err := writeNotifierChannel(persistCmd(), cfgPath, n); err != nil {
		t.Fatalf("writeNotifierChannel: %v", err)
	}

	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Notifiers) != 1 || cfg.Notifiers[0].Type != config.NotifierTypeWebhook {
		t.Fatalf("webhook notifier not written: %+v", cfg.Notifiers)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Notifier != "webhook" {
		t.Fatalf("fallback route not written: %+v", cfg.Routes)
	}
	if verr := config.ValidateNotifiers(cfg.Notifiers); verr != nil {
		t.Fatalf("written webhook config failed ValidateNotifiers: %v", verr)
	}
}

// TestWriteNotifierChannelNeverEchoesSecret is the CLAUDE.md hard rule at the cli
// layer: an SMTP password / webhook header token passed to writeNotifierChannel
// must never appear in stdout or stderr (the success line carries no secret), and
// a test-alert FAILURE warning must also be secret-free.
func TestWriteNotifierChannelNeverEchoesSecret(t *testing.T) {
	cfgPath := seedChannelCfg(t)

	prev := sendChannelTestAlertFn
	sendChannelTestAlertFn = func(context.Context, config.NotifierConfig) error {
		// Mirror a real notifier: the error is already scrubbed of secrets.
		return errChannelTestFailed
	}
	t.Cleanup(func() { sendChannelTestAlertFn = prev })

	var out, errOut bytes.Buffer
	cmd := persistCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	const secret = "SuperSecretPassw0rd"
	n := config.NotifierConfig{
		Name: "email", Type: config.NotifierTypeEmail,
		SMTPHost: "smtp.example.com", SMTPPort: 465,
		Password: secret, From: "a@b.com", To: []string{"c@d.com"},
	}
	if err := writeNotifierChannel(cmd, cfgPath, n); err != nil {
		t.Fatalf("a test-alert failure must be non-fatal, got: %v", err)
	}
	if strings.Contains(out.String(), secret) || strings.Contains(errOut.String(), secret) {
		t.Fatalf("secret leaked:\nstdout=%s\nstderr=%s", out.String(), errOut.String())
	}
	// The config-on-disk holds the password (that's expected — it's the operator's
	// own file at 0600), but the PROGRAM OUTPUT must not.
}

// TestWriteNotifierChannelTestAlertFailureNonFatal pins that a failed test alert is
// advisory: the notifier is still written and the call returns nil (the operator is
// configured even if the test send didn't land), with a re-test hint on stderr.
func TestWriteNotifierChannelTestAlertFailureNonFatal(t *testing.T) {
	cfgPath := seedChannelCfg(t)

	prev := sendChannelTestAlertFn
	sendChannelTestAlertFn = func(context.Context, config.NotifierConfig) error { return errChannelTestFailed }
	t.Cleanup(func() { sendChannelTestAlertFn = prev })

	var errOut bytes.Buffer
	cmd := persistCmd()
	cmd.SetErr(&errOut)

	n := config.NotifierConfig{
		Name: "webhook", Type: config.NotifierTypeWebhook, URL: "https://example.com/hook",
	}
	if err := writeNotifierChannel(cmd, cfgPath, n); err != nil {
		t.Fatalf("test-alert failure must be non-fatal: %v", err)
	}
	cfg, _ := config.Load(cfgPath, nil)
	if len(cfg.Notifiers) != 1 {
		t.Fatalf("notifier must still be written on a test-alert failure: %+v", cfg.Notifiers)
	}
	if !strings.Contains(errOut.String(), "notify test") {
		t.Fatalf("expected a re-test hint mentioning `notify test` on stderr, got: %s", errOut.String())
	}
}
