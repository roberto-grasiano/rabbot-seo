package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadMergeOrder(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, "control:\n  port: 8888\ncrawler:\n  contact_email: ops@example.com\n")

	// env overrides the yaml value for control.port
	t.Setenv("RABBOT_CONTROL__PORT", "9999")

	c, err := Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if c.Control.Port != 9999 {
		t.Errorf("control.port = %d, want 9999 (env should override yaml)", c.Control.Port)
	}
	if c.Crawler.ContactEmail != "ops@example.com" {
		t.Errorf("contact_email = %q, want yaml value", c.Crawler.ContactEmail)
	}
	// untouched key keeps its default
	if c.Defaults.PerHostConcurrency != 2 {
		t.Errorf("per_host_concurrency = %d, want default 2", c.Defaults.PerHostConcurrency)
	}
}

func TestLoadDataDirEnvVar(t *testing.T) {
	// RABBOT_DATA_DIR (single underscore) maps to the data_dir key.
	t.Setenv("RABBOT_DATA_DIR", "/srv/rabbot")
	c, err := Load(filepath.Join(t.TempDir(), "absent.yaml"), nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if c.DataDir != "/srv/rabbot" {
		t.Errorf("data_dir = %q, want /srv/rabbot (RABBOT_DATA_DIR)", c.DataDir)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"), nil)
	if err != nil {
		t.Fatalf("Load with missing file should not error, got: %v", err)
	}
	if c.Control.Port != 7777 {
		t.Errorf("control.port = %d, want default 7777", c.Control.Port)
	}
}

func TestInterpolateSecrets(t *testing.T) {
	t.Setenv("MY_HOOK", "https://hooks.example/secret")
	got := interpolate("${MY_HOOK}")
	if got != "https://hooks.example/secret" {
		t.Errorf("interpolate = %q, want resolved env value", got)
	}
	if interpolate("no-vars") != "no-vars" {
		t.Errorf("interpolate should pass through plain strings")
	}
}

// TestLoadInterpolatesSecrets is the round-trip test: a config.yaml carrying
// ${VAR} placeholders in secret-bearing fields must resolve those against the
// environment by the time Load returns, while non-secret fields stay verbatim.
func TestLoadInterpolatesSecrets(t *testing.T) {
	t.Setenv("SLACK_WEBHOOK_CRITICAL", "https://hooks.slack.example/critical")
	t.Setenv("EXAMPLE_WAF_TOKEN", "waf-secret-token")
	t.Setenv("EXAMPLE_BASIC_PASS", "hunter2")
	t.Setenv("EXAMPLE_COOKIE", "session-abc")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, `crawler:
  contact_email: ops@example.com
notifiers:
  - name: slack-critical
    type: slack-webhook
    url: ${SLACK_WEBHOOK_CRITICAL}
sites:
  - url: https://example.com
    name: Example
    segments:
      - name: blog
        match: "^/blog/"
    access:
      headers:
        X-Rabbot-Token: ${EXAMPLE_WAF_TOKEN}
      basic_user: admin
      basic_pass: ${EXAMPLE_BASIC_PASS}
      cookies:
        sid: ${EXAMPLE_COOKIE}
`)

	c, err := Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if len(c.Notifiers) != 1 {
		t.Fatalf("got %d notifiers, want 1", len(c.Notifiers))
	}
	if c.Notifiers[0].URL != "https://hooks.slack.example/critical" {
		t.Errorf("notifier url = %q, want interpolated env value", c.Notifiers[0].URL)
	}

	if len(c.Sites) != 1 {
		t.Fatalf("got %d sites, want 1", len(c.Sites))
	}
	site := c.Sites[0]
	if site.Access.Headers["X-Rabbot-Token"] != "waf-secret-token" {
		t.Errorf("header token = %q, want interpolated env value", site.Access.Headers["X-Rabbot-Token"])
	}
	if site.Access.BasicPass != "hunter2" {
		t.Errorf("basic_pass = %q, want interpolated env value", site.Access.BasicPass)
	}
	if site.Access.Cookies["sid"] != "session-abc" {
		t.Errorf("cookie sid = %q, want interpolated env value", site.Access.Cookies["sid"])
	}
	// Non-secret field must be left verbatim (no $-mangling).
	if len(site.Segments) != 1 || site.Segments[0].Match != "^/blog/" {
		t.Errorf("segment match = %v, want verbatim ^/blog/", site.Segments)
	}
}

// TestInterpolateSecretsExpandsNewFields is the round-trip for the A1 channels:
// ${ENV} placeholders in the email password and the webhook header values resolve
// against the environment by the time Load returns, exactly like the slack URL
// already does. Non-secret notifier fields (smtp_host, from) stay verbatim.
func TestInterpolateSecretsExpandsNewFields(t *testing.T) {
	t.Setenv("RABBOT_SMTP_PASS", "smtp-hunter2")
	t.Setenv("RABBOT_GLUE_WEBHOOK", "https://glue.example/secret-hook")
	t.Setenv("RABBOT_GLUE_TOKEN", "Bearer glue-secret")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, `crawler:
  contact_email: ops@example.com
notifiers:
  - name: ops-mail
    type: email-smtp
    smtp_host: smtp.example.com
    smtp_port: 465
    username: alerts@example.com
    password: ${RABBOT_SMTP_PASS}
    from: rabbot@example.com
    to: [seo-team@example.com]
  - name: glue
    type: generic-webhook
    url: ${RABBOT_GLUE_WEBHOOK}
    headers:
      Authorization: ${RABBOT_GLUE_TOKEN}
      X-Static: literal-value
`)
	c, err := Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Notifiers) != 2 {
		t.Fatalf("got %d notifiers, want 2", len(c.Notifiers))
	}
	mail, glue := c.Notifiers[0], c.Notifiers[1]
	if mail.Password != "smtp-hunter2" {
		t.Errorf("email password = %q, want interpolated env value", mail.Password)
	}
	// Non-secret fields are verbatim (not mangled, not blanked).
	if mail.SMTPHost != "smtp.example.com" || mail.From != "rabbot@example.com" {
		t.Errorf("non-secret email fields altered: host=%q from=%q", mail.SMTPHost, mail.From)
	}
	if glue.URL != "https://glue.example/secret-hook" {
		t.Errorf("webhook url = %q, want interpolated env value", glue.URL)
	}
	if glue.Headers["Authorization"] != "Bearer glue-secret" {
		t.Errorf("webhook Authorization header = %q, want interpolated env value", glue.Headers["Authorization"])
	}
	if glue.Headers["X-Static"] != "literal-value" {
		t.Errorf("static header altered: %q", glue.Headers["X-Static"])
	}
}

// TestLoadInterpolatesMissingEnvToEmpty pins os.Expand semantics: an unset
// ${VAR} resolves to the empty string (not the literal placeholder).
func TestLoadInterpolatesMissingEnvToEmpty(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, `crawler:
  contact_email: ops@example.com
notifiers:
  - name: slack-critical
    type: slack-webhook
    url: ${DEFINITELY_UNSET_WEBHOOK_VAR}
`)
	c, err := Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if len(c.Notifiers) != 1 {
		t.Fatalf("got %d notifiers, want 1", len(c.Notifiers))
	}
	if c.Notifiers[0].URL != "" {
		t.Errorf("notifier url = %q, want empty string for unset env var", c.Notifiers[0].URL)
	}
}
