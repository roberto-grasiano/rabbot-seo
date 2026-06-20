package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestNotifierConfigEmailRoundTrips proves the new email-smtp per-type fields are
// parsed from YAML with the public, spec-pinned tags (smtp_host, smtp_port,
// username, password, from, to, allow_plaintext).
func TestNotifierConfigEmailRoundTrips(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, `crawler:
  contact_email: ops@example.com
notifiers:
  - name: ops-mail
    type: email-smtp
    smtp_host: smtp.fastmail.com
    smtp_port: 465
    username: alerts@example.com
    password: s3cr3t
    from: rabbot@example.com
    to: [seo-team@example.com, oncall@example.com]
    allow_plaintext: false
`)
	c, err := Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Notifiers) != 1 {
		t.Fatalf("got %d notifiers, want 1", len(c.Notifiers))
	}
	n := c.Notifiers[0]
	if n.Name != "ops-mail" || n.Type != "email-smtp" {
		t.Fatalf("name/type = %q/%q, want ops-mail/email-smtp", n.Name, n.Type)
	}
	if n.SMTPHost != "smtp.fastmail.com" {
		t.Errorf("smtp_host = %q", n.SMTPHost)
	}
	if n.SMTPPort != 465 {
		t.Errorf("smtp_port = %d, want 465", n.SMTPPort)
	}
	if n.Username != "alerts@example.com" {
		t.Errorf("username = %q", n.Username)
	}
	if n.Password != "s3cr3t" {
		t.Errorf("password = %q", n.Password)
	}
	if n.From != "rabbot@example.com" {
		t.Errorf("from = %q", n.From)
	}
	if len(n.To) != 2 || n.To[0] != "seo-team@example.com" || n.To[1] != "oncall@example.com" {
		t.Errorf("to = %v, want both recipients", n.To)
	}
	if n.AllowPlaintext {
		t.Errorf("allow_plaintext = true, want false")
	}
}

// TestNotifierConfigWebhookRoundTrips proves the generic-webhook per-type fields
// (url + static headers map) parse from YAML.
func TestNotifierConfigWebhookRoundTrips(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, `crawler:
  contact_email: ops@example.com
notifiers:
  - name: glue
    type: generic-webhook
    url: https://glue.example/hook
    headers:
      Authorization: Bearer abc123
      X-Source: rabbot
`)
	c, err := Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Notifiers) != 1 {
		t.Fatalf("got %d notifiers, want 1", len(c.Notifiers))
	}
	n := c.Notifiers[0]
	if n.Type != "generic-webhook" || n.URL != "https://glue.example/hook" {
		t.Fatalf("type/url = %q/%q", n.Type, n.URL)
	}
	if n.Headers["Authorization"] != "Bearer abc123" {
		t.Errorf("Authorization header = %q", n.Headers["Authorization"])
	}
	if n.Headers["X-Source"] != "rabbot" {
		t.Errorf("X-Source header = %q", n.Headers["X-Source"])
	}
}

// TestValidateNotifiersAcceptsKnownTypes confirms a fully-specified notifier of
// each settled public type validates clean.
func TestValidateNotifiersAcceptsKnownTypes(t *testing.T) {
	cfg := []NotifierConfig{
		{Name: "slk", Type: "slack-webhook", URL: "https://hooks.slack.com/services/T/B/X"},
		{
			Name: "ops-mail", Type: "email-smtp",
			SMTPHost: "smtp.example.com", SMTPPort: 587,
			From: "rabbot@example.com", To: []string{"team@example.com"},
		},
		{Name: "glue", Type: "generic-webhook", URL: "https://glue.example/hook"},
	}
	if err := ValidateNotifiers(cfg); err != nil {
		t.Fatalf("ValidateNotifiers rejected valid config: %v", err)
	}
}

// TestValidateNotifiersRejectsUnknownType requires a helpful error that names the
// offending notifier and LISTS the valid types.
func TestValidateNotifiersRejectsUnknownType(t *testing.T) {
	err := ValidateNotifiers([]NotifierConfig{{Name: "pigeon", Type: "carrier-pigeon"}})
	if err == nil {
		t.Fatal("expected an error for an unknown notifier type")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pigeon") {
		t.Errorf("error should name the notifier; got %q", msg)
	}
	if !strings.Contains(msg, "carrier-pigeon") {
		t.Errorf("error should name the bad type; got %q", msg)
	}
	// Must list every valid type so the operator can self-correct.
	for _, want := range []string{"slack-webhook", "email-smtp", "generic-webhook"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should list valid type %q; got %q", want, msg)
		}
	}
}

// TestValidateNotifiersRequiresFieldsPerType pins the required-field contract per
// type: email-smtp needs smtp_host/from/to; generic-webhook and slack-webhook need
// url. Each error names the missing field and the notifier, and NEVER echoes a
// secret value.
func TestValidateNotifiersRequiresFieldsPerType(t *testing.T) {
	const pw = "TOP-SECRET-PW"
	tests := []struct {
		name        string
		n           NotifierConfig
		wantMissing []string
	}{
		{
			name:        "email missing host/from/to",
			n:           NotifierConfig{Name: "m", Type: "email-smtp", Password: pw},
			wantMissing: []string{"smtp_host", "from", "to"},
		},
		{
			name:        "email missing only to",
			n:           NotifierConfig{Name: "m", Type: "email-smtp", SMTPHost: "h", From: "f@x.com", Password: pw},
			wantMissing: []string{"to"},
		},
		{
			name:        "webhook missing url",
			n:           NotifierConfig{Name: "w", Type: "generic-webhook", Headers: map[string]string{"Authorization": pw}},
			wantMissing: []string{"url"},
		},
		{
			name:        "slack missing url",
			n:           NotifierConfig{Name: "s", Type: "slack-webhook"},
			wantMissing: []string{"url"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNotifiers([]NotifierConfig{tc.n})
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.n.Name) {
				t.Errorf("error should name the notifier %q; got %q", tc.n.Name, msg)
			}
			for _, f := range tc.wantMissing {
				if !strings.Contains(msg, f) {
					t.Errorf("error should name missing field %q; got %q", f, msg)
				}
			}
			// Hard rule: a secret value must NEVER appear in a validation error.
			if strings.Contains(msg, pw) {
				t.Errorf("validation error leaked a secret value: %q", msg)
			}
		})
	}
}

// TestValidateNotifiersPortRules enforces sane SMTP port bounds for email-smtp: a
// port outside 1..65535 is rejected (0 means "unset" and is also rejected as a
// required field, since a TLS-mode decision needs a real port).
func TestValidateNotifiersPortRules(t *testing.T) {
	base := func(port int) NotifierConfig {
		return NotifierConfig{
			Name: "m", Type: "email-smtp", SMTPHost: "h",
			From: "f@x.com", To: []string{"t@x.com"}, SMTPPort: port,
		}
	}
	tests := []struct {
		name string
		port int
		ok   bool
	}{
		{"465 ok", 465, true},
		{"587 ok", 587, true},
		{"25 ok", 25, true},
		{"max ok", 65535, true},
		{"zero rejected", 0, false},
		{"negative rejected", -1, false},
		{"too large rejected", 70000, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNotifiers([]NotifierConfig{base(tc.port)})
			if tc.ok && err != nil {
				t.Errorf("port %d should be accepted, got %v", tc.port, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("port %d should be rejected", tc.port)
			}
			if !tc.ok && err != nil && !strings.Contains(err.Error(), "smtp_port") {
				t.Errorf("bad-port error should name smtp_port; got %q", err.Error())
			}
		})
	}
}

// TestValidateFoldsInNotifiers proves Config.Validate runs notifier validation, so
// `rabbot config validate` / `rabbot doctor` catch a bad type or missing field.
func TestValidateFoldsInNotifiers(t *testing.T) {
	c := Defaults()
	c.Crawler.ContactEmail = "ops@example.com"
	c.Notifiers = []NotifierConfig{{Name: "x", Type: "totally-bogus"}}
	if err := c.Validate(); err == nil {
		t.Fatal("Config.Validate should reject a bad notifier type")
	}
}
