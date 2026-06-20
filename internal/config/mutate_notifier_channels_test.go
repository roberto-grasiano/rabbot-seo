package config

import (
	"os"
	"strings"
	"testing"
)

// TestAddNotifierYAMLWritesEmailFields pins that AddNotifierYAML writes the
// email-smtp per-type fields (smtp_host, smtp_port, username, from, to, and
// allow_plaintext) so the onboarding wizard can configure an email channel. The
// written config must round-trip through Load AND pass ValidateNotifiers (the same
// gate the daemon enforces), proving the wizard never writes an unloadable notifier.
func TestAddNotifierYAMLWritesEmailFields(t *testing.T) {
	path := seedConfig(t)

	in := NotifierConfig{
		Name:           "ops-mail",
		Type:           NotifierTypeEmail,
		SMTPHost:       "smtp.example.com",
		SMTPPort:       587,
		Username:       "alerts@example.com",
		Password:       "${RABBOT_SMTP_PASS}",
		From:           "rabbot@example.com",
		To:             []string{"seo@example.com", "lead@example.com"},
		AllowPlaintext: true,
	}
	if err := AddNotifierYAML(path, in); err != nil {
		t.Fatalf("AddNotifierYAML(email): %v", err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Notifiers) != 1 {
		t.Fatalf("want 1 notifier, got %d: %+v", len(cfg.Notifiers), cfg.Notifiers)
	}
	n := cfg.Notifiers[0]
	if n.Name != "ops-mail" || n.Type != NotifierTypeEmail {
		t.Fatalf("name/type wrong: %+v", n)
	}
	if n.SMTPHost != "smtp.example.com" || n.SMTPPort != 587 {
		t.Fatalf("smtp host/port not round-tripped: host=%q port=%d", n.SMTPHost, n.SMTPPort)
	}
	if n.Username != "alerts@example.com" || n.From != "rabbot@example.com" {
		t.Fatalf("username/from not round-tripped: %+v", n)
	}
	if len(n.To) != 2 || n.To[0] != "seo@example.com" || n.To[1] != "lead@example.com" {
		t.Fatalf("to[] not round-tripped: %+v", n.To)
	}
	if !n.AllowPlaintext {
		t.Fatalf("allow_plaintext not round-tripped: %+v", n)
	}
	// The config the wizard writes MUST pass the daemon's validation gate.
	if verr := ValidateNotifiers(cfg.Notifiers); verr != nil {
		t.Fatalf("wizard-written email config failed ValidateNotifiers: %v", verr)
	}
}

// TestAddNotifierYAMLPreservesEmailPasswordEnvToken pins that the SMTP password
// ${ENV} token is written to disk LITERALLY (never expanded at write time), so the
// secret can live outside config.yaml and koanf expands it at Load. This is the
// secret-out-of-file guarantee for email, mirroring the slack URL behavior.
func TestAddNotifierYAMLPreservesEmailPasswordEnvToken(t *testing.T) {
	path := seedConfig(t)

	const token = "${RABBOT_SMTP_PASS}"
	in := NotifierConfig{
		Name: "ops-mail", Type: NotifierTypeEmail,
		SMTPHost: "smtp.example.com", SMTPPort: 465,
		From: "a@b.com", To: []string{"c@d.com"}, Password: token,
	}
	if err := AddNotifierYAML(path, in); err != nil {
		t.Fatalf("AddNotifierYAML: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), token) {
		t.Fatalf("password env-token not preserved literally on disk:\n%s", raw)
	}
}

// TestAddNotifierYAMLWritesWebhookHeaders pins that AddNotifierYAML writes the
// generic-webhook url + static headers, and that an ${ENV} token in a header VALUE
// (e.g. Authorization) survives to disk literally for env-interpolation at Load.
func TestAddNotifierYAMLWritesWebhookHeaders(t *testing.T) {
	path := seedConfig(t)

	const authToken = "${RABBOT_GLUE_TOKEN}"
	in := NotifierConfig{
		Name: "glue", Type: NotifierTypeWebhook,
		URL:     "https://example.com/hook",
		Headers: map[string]string{"Authorization": authToken},
	}
	if err := AddNotifierYAML(path, in); err != nil {
		t.Fatalf("AddNotifierYAML(webhook): %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// The token is written to disk LITERALLY (never expanded at write time).
	if !strings.Contains(string(raw), authToken) {
		t.Fatalf("header env-token not preserved literally on disk:\n%s", raw)
	}

	// With the env var SET, Load must interpolate the header value (proving the
	// secret-out-of-file path Wave 2a wired in interpolateSecrets works for header
	// VALUES, not just the slack URL).
	t.Setenv("RABBOT_GLUE_TOKEN", "Bearer s3cr3t")
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Notifiers) != 1 {
		t.Fatalf("want 1 notifier, got %d", len(cfg.Notifiers))
	}
	n := cfg.Notifiers[0]
	if n.Type != NotifierTypeWebhook || n.URL != "https://example.com/hook" {
		t.Fatalf("webhook type/url wrong: %+v", n)
	}
	if got := n.Headers["Authorization"]; got != "Bearer s3cr3t" {
		t.Fatalf("header value not interpolated at Load: %q", got)
	}
	if verr := ValidateNotifiers(cfg.Notifiers); verr != nil {
		t.Fatalf("wizard-written webhook config failed ValidateNotifiers: %v", verr)
	}
}

// TestAddNotifierYAMLEmailIdempotentByName pins that re-adding an email notifier
// with the same name REPLACES it in place (no duplicate), so a re-run of the wizard
// that re-collects email settings keeps exactly one notifier — and the replacement
// fully overwrites the per-type fields (stale fields from the prior write are gone).
func TestAddNotifierYAMLEmailIdempotentByName(t *testing.T) {
	path := seedConfig(t)

	first := NotifierConfig{
		Name: "ops-mail", Type: NotifierTypeEmail,
		SMTPHost: "old.example.com", SMTPPort: 25,
		From: "a@b.com", To: []string{"c@d.com"},
	}
	if err := AddNotifierYAML(path, first); err != nil {
		t.Fatalf("add first: %v", err)
	}
	second := NotifierConfig{
		Name: "ops-mail", Type: NotifierTypeEmail,
		SMTPHost: "new.example.com", SMTPPort: 465,
		From: "x@y.com", To: []string{"z@w.com"},
	}
	if err := AddNotifierYAML(path, second); err != nil {
		t.Fatalf("add second: %v", err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Notifiers) != 1 {
		t.Fatalf("want exactly 1 notifier after same-name re-add, got %d: %+v", len(cfg.Notifiers), cfg.Notifiers)
	}
	n := cfg.Notifiers[0]
	if n.SMTPHost != "new.example.com" || n.SMTPPort != 465 || n.From != "x@y.com" {
		t.Fatalf("replacement did not fully overwrite per-type fields: %+v", n)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "old.example.com") {
		t.Fatalf("stale field from first write survived on disk:\n%s", raw)
	}
}
