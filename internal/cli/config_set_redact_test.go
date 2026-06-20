package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestConfigSetSuccessLineRedactsValue pins finding #20.1: the `config set`
// success echo must surface ONLY the key, never the value. A value can be a
// secret (e.g. a Slack webhook URL like notifiers.0.url=https://hooks.slack.com/
// services/T.../B.../XXXX); CLAUDE.md forbids surfacing webhook URLs/tokens, and
// the acknowledgement is written to stdout where it may be logged or shared.
func TestConfigSetSuccessLineRedactsValue(t *testing.T) {
	const (
		key = "notifiers.0.url"
		// Example only — intentionally NOT a real-shaped webhook token, so secret
		// scanners (GitHub push protection) don't false-positive on a test fixture.
		secret = "https://hooks.slack.com/services/TEXAMPLE/BEXAMPLE/not-a-real-webhook-token"
	)
	var out bytes.Buffer
	if err := configSetSuccessLine(&out, key+"="+secret); err != nil {
		t.Fatalf("configSetSuccessLine: %v", err)
	}
	got := out.String()

	// The key must be acknowledged so the operator sees what changed.
	if !strings.Contains(got, key) {
		t.Errorf("success line %q does not contain the key %q", got, key)
	}
	// The value (secret) must NOT appear anywhere in the output. Assert on the
	// whole secret and on its discriminating substrings so a partial leak (host
	// or token) is still caught.
	for _, leak := range []string{secret, "hooks.slack.com", "not-a-real-webhook-token", "TEXAMPLE"} {
		if strings.Contains(got, leak) {
			t.Errorf("success line %q leaks secret fragment %q", got, leak)
		}
	}
}

// TestConfigSetSuccessLineRedactsNewNotifierSecrets extends the never-leak
// guarantee to the A1 channel secrets: the `config set` success echo for an email
// password, a generic-webhook URL, or a static Authorization header value must
// surface the KEY only, never the value. (config set of a notifiers.* key is in
// fact denied over the control plane, so this is defense in depth on the echo
// itself — the redaction is uniform regardless of which key was set.)
func TestConfigSetSuccessLineRedactsNewNotifierSecrets(t *testing.T) {
	tests := []struct {
		key    string
		secret string
		leaks  []string
	}{
		{
			key:    "notifiers.0.password",
			secret: "smtp-hunter2-secret",
			leaks:  []string{"smtp-hunter2-secret", "hunter2"},
		},
		{
			key:    "notifiers.1.url",
			secret: "https://glue.example/hook?token=GLUE-SECRET-TOKEN",
			leaks:  []string{"GLUE-SECRET-TOKEN", "glue.example"},
		},
		{
			key:    "notifiers.1.headers.Authorization",
			secret: "Bearer GLUE-AUTH-SECRET",
			leaks:  []string{"GLUE-AUTH-SECRET", "Bearer"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			var out bytes.Buffer
			if err := configSetSuccessLine(&out, tc.key+"="+tc.secret); err != nil {
				t.Fatalf("configSetSuccessLine: %v", err)
			}
			got := out.String()
			if !strings.Contains(got, tc.key) {
				t.Errorf("success line %q does not contain the key %q", got, tc.key)
			}
			for _, leak := range tc.leaks {
				if strings.Contains(got, leak) {
					t.Errorf("success line %q leaks secret fragment %q", got, leak)
				}
			}
		})
	}
}

// TestConfigSetSuccessLineKeyOnly confirms the success line for a non-secret
// key=value still prints just the key (no value), so the redaction is uniform
// and not special-cased to secret-looking keys.
func TestConfigSetSuccessLineKeyOnly(t *testing.T) {
	var out bytes.Buffer
	if err := configSetSuccessLine(&out, "log.level=debug"); err != nil {
		t.Fatalf("configSetSuccessLine: %v", err)
	}
	got := strings.TrimSpace(out.String())
	if got != "set log.level" {
		t.Errorf("success line = %q, want %q (key only, value elided)", got, "set log.level")
	}
}
