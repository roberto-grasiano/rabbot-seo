package cli

import (
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// TestResolveConfigGet asserts config get is symmetric with config set: every key
// settable via config set (the config.AllowConfigKey allow-list) is also readable
// via config get, plus the legacy read-only keys. Secrets/denied keys stay
// unreadable.
func TestResolveConfigGet(t *testing.T) {
	mp := 500
	cfg := &config.Config{
		DataDir: "/var/data",
	}
	cfg.Control.Port = 7777
	cfg.Log.Level = "debug"
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Defaults.MinInterval = "10m"
	cfg.Defaults.MaxInterval = "24h"
	cfg.Defaults.Discovery.MaxPagesPerSite = &mp

	tests := []struct {
		key   string
		want  string
		found bool
	}{
		// Legacy read-only keys stay readable.
		{"control.port", "7777", true},
		{"log.level", "debug", true},
		{"data_dir", "/var/data", true},
		{"crawler.contact_email", "ops@example.com", true},
		// Allow-list (settable) keys are now ALSO gettable — the symmetry fix.
		{"defaults.min_interval", "10m", true},
		{"defaults.max_interval", "24h", true},
		{"defaults.discovery.max_pages_per_site", "500", true},
		// Unknown / secret-family keys remain unreadable.
		{"notifiers.0.url", "", false},
		{"defaults.unverified_throttle.min_interval", "", false},
		{"bogus.key", "", false},
	}

	for _, tt := range tests {
		got, found, err := resolveConfigGet(cfg, tt.key)
		if err != nil {
			t.Errorf("resolveConfigGet(%q) unexpected err: %v", tt.key, err)
			continue
		}
		if found != tt.found {
			t.Errorf("resolveConfigGet(%q) found = %v, want %v", tt.key, found, tt.found)
			continue
		}
		if found && got != tt.want {
			t.Errorf("resolveConfigGet(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

// TestConfigGetCoversAllSettableKeys is the drift guard: every key settable via
// config set (config.AllowConfigKey accepts it) MUST also be gettable, or get/set
// are asymmetric again. If the allow-list grows, this test fails until the new key
// is added to resolveConfigGet.
func TestConfigGetCoversAllSettableKeys(t *testing.T) {
	settable := []string{
		"log.level",
		"defaults.min_interval",
		"defaults.max_interval",
		"defaults.discovery.max_pages_per_site",
	}
	cfg := &config.Config{}
	for _, key := range settable {
		if err := config.AllowConfigKey(key); err != nil {
			t.Fatalf("test fixture drift: %q is no longer settable: %v", key, err)
		}
		_, found, err := resolveConfigGet(cfg, key)
		if err != nil {
			t.Errorf("resolveConfigGet(%q) err: %v", key, err)
			continue
		}
		if !found {
			t.Errorf("settable key %q is not gettable — config get/set are asymmetric", key)
		}
	}
}

// TestResolveConfigGetNeverReturnsNotifierSecrets is the never-leak regression for
// the A1 channels: the new secret-bearing notifier fields (email password, webhook
// URL, and a static Authorization header value) must be UNREADABLE via `config
// get`, exactly like the slack notifiers.0.url already is — they fall through to
// found=false so a secret can never be printed to stdout. (Defense in depth atop
// the control-plane deny of notifiers.* and the absence of these keys from the get
// allow-list.)
func TestResolveConfigGetNeverReturnsNotifierSecrets(t *testing.T) {
	const (
		smtpPW    = "smtp-top-secret"
		hookURL   = "https://hooks.slack.com/services/T0/B0/SECRETTOKEN"
		genURL    = "https://glue.example/hook?key=GLUE-SECRET"
		authValue = "Bearer GLUE-AUTH-SECRET"
	)
	cfg := &config.Config{
		Notifiers: []config.NotifierConfig{
			{
				Name: "ops-mail", Type: "email-smtp",
				SMTPHost: "smtp.example.com", SMTPPort: 465,
				From: "rabbot@example.com", To: []string{"team@example.com"},
				Username: "alerts@example.com", Password: smtpPW,
			},
			{Name: "slack", Type: "slack-webhook", URL: hookURL},
			{
				Name: "glue", Type: "generic-webhook", URL: genURL,
				Headers: map[string]string{"Authorization": authValue},
			},
		},
	}
	secretKeys := []string{
		"notifiers.0.password",
		"notifiers.0.username",
		"notifiers.1.url",
		"notifiers.2.url",
		"notifiers.2.headers.Authorization",
	}
	for _, key := range secretKeys {
		value, found, err := resolveConfigGet(cfg, key)
		if err != nil {
			t.Errorf("resolveConfigGet(%q) err: %v", key, err)
			continue
		}
		if found {
			t.Errorf("secret key %q is readable via config get (value %q) — must be unreadable", key, value)
		}
	}
	// And no secret substring escapes regardless of the (false) found result.
	for _, leak := range []string{smtpPW, "SECRETTOKEN", "GLUE-SECRET", "GLUE-AUTH-SECRET"} {
		for _, key := range secretKeys {
			if v, _, _ := resolveConfigGet(cfg, key); strings.Contains(v, leak) {
				t.Errorf("config get(%q) leaked secret fragment %q", key, leak)
			}
		}
	}
}

// TestResolveConfigGetUnsetSettableKey asserts a settable key with no explicit
// value (max_pages_per_site is a *int that is nil when unset) reads back as an
// empty/sentinel string but is still reported as a known key, never an error.
func TestResolveConfigGetUnsetMaxPages(t *testing.T) {
	cfg := &config.Config{}
	got, found, err := resolveConfigGet(cfg, "defaults.discovery.max_pages_per_site")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !found {
		t.Fatalf("max_pages_per_site should be a known key even when unset")
	}
	if got != "" {
		t.Errorf("unset max_pages_per_site = %q, want empty string", got)
	}
}
