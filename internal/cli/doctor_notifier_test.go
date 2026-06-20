package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
)

// TestNotifierDoctorLine pins the doctor channel-status rendering (decision #23):
// zero notifiers → a warning-tone line that names the zero-channel state and points
// at a fix; one-or-more → a neutral "configured" line that reports the count. It is
// never a failure verdict (alerts are optional).
func TestNotifierDoctorLine(t *testing.T) {
	// Zero channels: warning tone, names the gap, suggests the wizard.
	zero := notifierDoctorLine(&config.Config{})
	low := strings.ToLower(zero)
	if !strings.Contains(low, "no") || !strings.Contains(low, "alert") {
		t.Fatalf("zero-channel doctor line should flag no alert channels: %q", zero)
	}
	if !strings.Contains(low, "rabbot init") {
		t.Fatalf("zero-channel doctor line should suggest `rabbot init`: %q", zero)
	}
	// Warning tone, not a hard failure marker.
	if strings.Contains(zero, "RED") || strings.Contains(zero, "FAIL") {
		t.Fatalf("zero-channel doctor line must be a warning, not a failure: %q", zero)
	}

	// Two channels: neutral, reports the count, no warning.
	two := notifierDoctorLine(&config.Config{
		Notifiers: []config.NotifierConfig{
			{Name: "slack", Type: config.NotifierTypeSlack, URL: "https://hooks.slack.com/x"},
			{Name: "mail", Type: config.NotifierTypeEmail, SMTPHost: "h", SMTPPort: 465, From: "a@b.com", To: []string{"c@d.com"}},
		},
	})
	if !strings.Contains(two, "2") {
		t.Fatalf("configured doctor line should report the channel count: %q", two)
	}
	if strings.Contains(strings.ToLower(two), "no alert channel") {
		t.Fatalf("a configured doctor line must not warn about zero channels: %q", two)
	}
}

// TestRunDoctorReportsZeroChannels is the integration check: `doctor` against a real
// (loopback) site with a notifier-less config prints the zero-channel line. cfg is
// non-nil so the line renders (and the control section runs harmlessly daemon-down).
func TestRunDoctorReportsZeroChannels(t *testing.T) {
	site := ssrServer(t)
	var buf bytes.Buffer
	// A minimal non-nil config with zero notifiers. Defaults are fine; we only read
	// Notifiers (empty) for the channel line.
	cfg := config.Defaults()
	err := runDoctor(context.Background(), &buf, site.URL, precheck.Options{
		UserAgent:    "Rabbot-SEO/test (+https://example.test)",
		AllowPrivate: true,
	}, &cfg)
	if err != nil {
		t.Fatalf("runDoctor() error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Alert channels:") {
		t.Fatalf("doctor output missing the Alert channels section:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "no alert channel") {
		t.Fatalf("doctor output missing the zero-channel warning line:\n%s", out)
	}
}
