package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSendTestAlertSuccess stands up an httptest server, points SendTestAlert at
// it via the injected client, and asserts exactly one POST whose JSON body
// carries the sample alert marker (ChangeType "notify_test").
func TestSendTestAlertSuccess(t *testing.T) {
	var hits int32
	var gotBody []byte
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = b
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	if err := SendTestAlert(context.Background(), srv.URL, srv.Client()); err != nil {
		t.Fatalf("SendTestAlert: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected exactly 1 POST, got %d", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(string(gotBody), "notify_test") {
		t.Fatalf("test-alert body missing the notify_test marker:\n%s", gotBody)
	}
}

// TestSendTestAlertFailureIsScrubbed points the webhook at a server returning 500
// and asserts the error is non-nil AND that it does NOT contain the webhook URL
// (the slackNotifier already redacts it, so a surfaced error is leak-safe).
func TestSendTestAlertFailureIsScrubbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	err := SendTestAlert(context.Background(), srv.URL, srv.Client())
	if err == nil {
		t.Fatal("expected an error from a 500 response, got nil")
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("error leaked the webhook URL: %v", err)
	}
}

// TestSendTestAlertEmptyWebhook asserts the empty-webhook case returns the
// sentinel ErrNoWebhook so callers can distinguish "nothing configured" from a
// genuine send failure.
func TestSendTestAlertEmptyWebhook(t *testing.T) {
	err := SendTestAlert(context.Background(), "", nil)
	if !errors.Is(err, ErrNoWebhook) {
		t.Fatalf("SendTestAlert(empty) = %v, want ErrNoWebhook", err)
	}
}

// TestSendTestAlertHonorsContextDeadline locks the contract the onboarding
// alerts step relies on: a bounded context bites mid-backoff. The server always
// returns a 429 with a large Retry-After, which would otherwise drive the slack
// retry loop for minutes; with a short-deadline context SendTestAlert must return
// PROMPTLY with a deadline-exceeded error rather than sleeping out the backoff.
// This is the regression guard for the cancellable slack backoff wait.
func TestSendTestAlertHonorsContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120") // far beyond any sane test runtime
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := SendTestAlert(ctx, srv.URL, srv.Client())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a context-deadline error when the backoff outlasts the deadline, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("SendTestAlert did not honor the context deadline promptly: took %s", elapsed)
	}
}
