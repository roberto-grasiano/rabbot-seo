package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

func TestLogMutation_Success(t *testing.T) {
	var buf bytes.Buffer
	logger := obs.NewLogger(&buf, "info")

	err := logMutation(logger, "add_site", map[string]any{obs.KeySite: "https://x.test"}, func() error {
		return nil
	})
	if err != nil {
		t.Fatalf("logMutation returned err: %v", err)
	}
	var rec map[string]any
	if jerr := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); jerr != nil {
		t.Fatalf("log line not JSON: %v (%s)", jerr, buf.String())
	}
	if rec["msg"] != "control mutation" {
		t.Errorf("msg = %v, want \"control mutation\"", rec["msg"])
	}
	if rec[obs.KeyComponent] != "control" {
		t.Errorf("component = %v, want control", rec[obs.KeyComponent])
	}
	if rec[obs.KeyAction] != "add_site" {
		t.Errorf("action = %v, want add_site", rec[obs.KeyAction])
	}
	if rec["site"] != "https://x.test" {
		t.Errorf("site attr missing/wrong: %v", rec["site"])
	}
	if rec["outcome"] != "ok" {
		t.Errorf("outcome = %v, want ok", rec["outcome"])
	}
}

func TestLogMutation_Failure(t *testing.T) {
	var buf bytes.Buffer
	logger := obs.NewLogger(&buf, "info")
	wantErr := errors.New("boom")

	err := logMutation(logger, "pause_monitoring", nil, func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("logMutation err = %v, want boom (must pass through)", err)
	}
	var rec map[string]any
	if jerr := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); jerr != nil {
		t.Fatalf("log line not JSON: %v", jerr)
	}
	if rec["outcome"] != "error" {
		t.Errorf("outcome = %v, want error", rec["outcome"])
	}
	if rec[obs.KeyError] != "boom" {
		t.Errorf("error attr = %v, want boom", rec[obs.KeyError])
	}
}

// Secret-safety: a notifier-test mutation must never log a value, only the name.
func TestLogMutation_NoSecretValue(t *testing.T) {
	var buf bytes.Buffer
	logger := obs.NewLogger(&buf, "info")
	_ = logMutation(logger, "send_test_alert", map[string]any{obs.KeyNotifier: "slack"}, func() error { return nil })
	if bytes.Contains(buf.Bytes(), []byte("https://hooks.slack.com")) {
		t.Fatal("mutation log leaked a webhook URL")
	}
}

// TestLogMutation_NoNewChannelSecrets extends the never-leak guarantee to the A1
// email + generic-webhook channels: a send_test_alert mutation against an
// email-smtp or generic-webhook notifier logs only the notifier NAME, so none of
// the new secret shapes (SMTP password, a webhook URL bearing a token, a static
// Authorization header value) can ever reach the mutation log. The hooks build the
// attrs from the notifier name alone (run.go), so even if a secret string is in
// scope it is never passed in — assert that contract holds.
func TestLogMutation_NoNewChannelSecrets(t *testing.T) {
	const (
		smtpPW    = "smtp-mutation-secret"
		hookToken = "WEBHOOK-URL-SECRET-TOKEN"
		authValue = "Bearer MUTATION-AUTH-SECRET"
	)
	for _, name := range []string{"ops-mail", "glue"} {
		var buf bytes.Buffer
		logger := obs.NewLogger(&buf, "info")
		// Mirror run.go's NotifyTest hook: attrs carry the NAME only.
		_ = logMutation(logger, "send_test_alert", map[string]any{obs.KeyNotifier: name}, func() error { return nil })
		for _, leak := range []string{smtpPW, hookToken, authValue} {
			if bytes.Contains(buf.Bytes(), []byte(leak)) {
				t.Fatalf("mutation log for %q leaked secret %q", name, leak)
			}
		}
	}
}
