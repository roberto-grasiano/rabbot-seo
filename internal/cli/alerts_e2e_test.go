package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// TestSendChannelTestAlert_WebhookDeliversByType proves the PRODUCTION onboarding
// test-alert path (sendChannelTestAlert) builds a generic-webhook notifier from a
// NotifierConfig and actually POSTs the synthetic alert — the same "deliver a sample
// alert by name/type" guarantee `rabbot notify test` provides at runtime, but inline
// at onboarding (the daemon is not up yet). It exercises the real notify constructor
// (no stub), so a regression that fails to wire the new type here would be caught.
func TestSendChannelTestAlert_WebhookDeliversByType(t *testing.T) {
	var hits int32
	var gotVersion int
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			PayloadVersion int `json:"payload_version"`
		}
		_ = json.Unmarshal(body, &payload)
		gotVersion = payload.PayloadVersion
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	n := config.NotifierConfig{
		Name: "webhook", Type: config.NotifierTypeWebhook,
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer test123"},
	}
	if err := sendChannelTestAlert(context.Background(), n); err != nil {
		t.Fatalf("sendChannelTestAlert(webhook): %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("want exactly 1 POST to the webhook endpoint, got %d", hits)
	}
	if gotVersion != 1 {
		t.Fatalf("want payload_version 1 (the wire contract), got %d", gotVersion)
	}
	if gotAuth != "Bearer test123" {
		t.Fatalf("static Authorization header not sent: %q", gotAuth)
	}
}

// TestSendChannelTestAlert_EmailBuildsNotifier proves the email branch constructs a
// real email-smtp notifier (so the by-type wiring is correct). A construction error
// (e.g. an incomplete config) would surface here; delivery itself is covered by the
// notify package's fake-SMTP tests. We point at an unroutable host with a cancelled
// context so the test never makes a real network call but still exercises the
// build-by-type path end to end.
func TestSendChannelTestAlert_EmailBuildsNotifier(t *testing.T) {
	n := config.NotifierConfig{
		Name: "email", Type: config.NotifierTypeEmail,
		SMTPHost: "127.0.0.1", SMTPPort: 2,
		From: "a@b.com", To: []string{"c@d.com"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // abort before any dial — we only assert the notifier was BUILT, not delivery

	err := sendChannelTestAlert(ctx, n)
	// A cancelled ctx or a dial failure is fine (no live server); a CONSTRUCTION
	// failure ("incomplete config") would be a wiring bug. Assert the error, if any,
	// is not a build/validation error.
	if err != nil && (strings.Contains(err.Error(), "incomplete config") ||
		strings.Contains(err.Error(), "unknown notifier type")) {
		t.Fatalf("email notifier was not built by type (wiring bug): %v", err)
	}
}
