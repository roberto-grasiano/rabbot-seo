package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// errRoundTripper fails every request so Notify exercises its error path. The
// stdlib http client wraps the returned error in a *url.Error whose Error()
// embeds the request URL (the secret for an Incoming Webhook).
type errRoundTripper struct{}

func (errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("transport boom")
}

func TestSlackNotifierPostsBlocks(t *testing.T) {
	var hits int32
	var gotBody []byte
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		mu.Lock()
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = buf
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	n := NewSlackNotifier("slack-critical", srv.URL, srv.Client())
	err := n.Notify(context.Background(), Alert{
		Site: "example.com", ChangeType: "indexability", Severity: model.SeverityCritical,
		Before: "indexable", After: "noindex", DetectedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("expected 1 webhook POST, got %d", hits)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotBody) == 0 {
		t.Error("expected a non-empty JSON body")
	}
}

func TestSlackNotifierHonorsRetryAfter(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0") // 0s so the test stays fast
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	n := NewSlackNotifier("s", srv.URL, srv.Client())
	if err := n.Notify(context.Background(), Alert{Site: "s", ChangeType: "title", Severity: model.SeverityWarning, DetectedAt: time.Now()}); err != nil {
		t.Fatalf("Notify after retry: %v", err)
	}
	if atomic.LoadInt32(&hits) < 2 {
		t.Errorf("expected a retry after 429, got %d hits", hits)
	}
}

// TestSlackNotifierRedactsWebhookURLOnError guards the redaction in Notify:
// the webhook URL IS the secret for an Incoming Webhook, so a transport failure
// must never leak it into the returned error string. The error must instead
// carry the "<redacted-webhook-url>" placeholder.
func TestSlackNotifierRedactsWebhookURLOnError(t *testing.T) {
	const secret = "T00000000-SUPERSECRETTOKEN-XYZ"
	webhookURL := "https://hooks.slack.com/services/" + secret

	client := &http.Client{Transport: errRoundTripper{}}
	n := NewSlackNotifier("slack-critical", webhookURL, client)

	err := n.Notify(context.Background(), Alert{
		Site: "example.com", ChangeType: "indexability", Severity: model.SeverityCritical,
		DetectedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("expected an error from a failing transport, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) {
		t.Errorf("error string leaked the webhook secret token %q: %s", secret, msg)
	}
	if strings.Contains(msg, webhookURL) {
		t.Errorf("error string leaked the full webhook URL: %s", msg)
	}
	if !strings.Contains(msg, "<redacted-webhook-url>") {
		t.Errorf("error string missing redaction placeholder, got: %s", msg)
	}
}

// TestSlackNotifierRetriesOn429WithoutRetryAfter guards F31: a Slack 429 that
// omits the Retry-After header surfaces from slack-go as a StatusCodeError
// (NOT *RateLimitedError), so the typed-RetryAfter path never fires. The
// notifier must still back off and retry instead of giving up after one attempt.
func TestSlackNotifierRetriesOn429WithoutRetryAfter(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			// No Retry-After header at all.
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	n := NewSlackNotifier("s", srv.URL, srv.Client())
	if err := n.Notify(context.Background(), Alert{Site: "s", ChangeType: "title", Severity: model.SeverityWarning, DetectedAt: time.Now()}); err != nil {
		t.Fatalf("Notify after headerless 429: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got < 2 {
		t.Errorf("expected a retry after a 429 without Retry-After, got %d hits", got)
	}
}

// TestSlackNotifierRetriesOn429WithHTTPDateRetryAfter guards F31: RFC 7231
// permits an HTTP-date Retry-After, which slack-go's strconv.ParseInt rejects,
// returning a raw (non-typed) error — also non-retryable via the *RateLimitedError
// path. The notifier must fall back to a default backoff and retry.
func TestSlackNotifierRetriesOn429WithHTTPDateRetryAfter(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT") // HTTP-date, not integer
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	n := NewSlackNotifier("s", srv.URL, srv.Client())
	if err := n.Notify(context.Background(), Alert{Site: "s", ChangeType: "title", Severity: model.SeverityWarning, DetectedAt: time.Now()}); err != nil {
		t.Fatalf("Notify after HTTP-date 429: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got < 2 {
		t.Errorf("expected a retry after a 429 with an HTTP-date Retry-After, got %d hits", got)
	}
}

// TestIsHTTPDateRetryAfter429TightensAgainstUnrelatedNumError guards issue #28:
// isHTTPDateRetryAfter429 must NOT treat *every* *strconv.NumError as a 429 with
// an HTTP-date Retry-After. slack-go parses the Retry-After header via
// strconv.ParseInt, so a genuine HTTP-date 429 yields a NumError with
// Func == "ParseInt" whose Num is the HTTP-date string. A NumError from any other
// integer-parse path (e.g. a future dep-bump that adds an Atoi/ParseFloat call
// inside PostWebhookCustomHTTPContext) is NOT a Retry-After parse and must be
// rejected — otherwise an unrelated parse failure would be misclassified as a
// retryable 429.
func TestIsHTTPDateRetryAfter429TightensAgainstUnrelatedNumError(t *testing.T) {
	httpDate := "Wed, 21 Oct 2026 07:28:00 GMT"

	// Reconstruct the exact error slack-go surfaces for an HTTP-date Retry-After:
	// strconv.ParseInt(<http-date>, 10, 64) fails with a *strconv.NumError whose
	// Func is "ParseInt" and Num is the HTTP-date string.
	_, realRetryAfterErr := strconv.ParseInt(httpDate, 10, 64)
	var realNumErr *strconv.NumError
	if !errors.As(realRetryAfterErr, &realNumErr) {
		t.Fatalf("expected ParseInt(%q) to yield *strconv.NumError, got %T", httpDate, realRetryAfterErr)
	}
	if realNumErr.Func != "ParseInt" {
		t.Fatalf("expected ParseInt NumError.Func == %q, got %q", "ParseInt", realNumErr.Func)
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "real HTTP-date Retry-After (ParseInt of an HTTP-date)",
			err:  realRetryAfterErr,
			want: true,
		},
		{
			// Wrapped, as slack-go / url.Error chains may wrap it; errors.As must
			// still find it and the heuristic must still match.
			name: "wrapped real HTTP-date Retry-After",
			err:  fmt.Errorf("post webhook: %w", realRetryAfterErr),
			want: true,
		},
		{
			// A NumError from Atoi — NOT a Retry-After parse. A future dep bump that
			// adds an Atoi call inside PostWebhookCustomHTTPContext must not trip the
			// heuristic.
			name: "Atoi-shaped NumError (unrelated parse path)",
			err:  &strconv.NumError{Func: "Atoi", Num: "42", Err: strconv.ErrSyntax},
			want: false,
		},
		{
			// A NumError from ParseFloat — also unrelated.
			name: "ParseFloat-shaped NumError (unrelated parse path)",
			err:  &strconv.NumError{Func: "ParseFloat", Num: "3.14", Err: strconv.ErrSyntax},
			want: false,
		},
		{
			// A ParseInt NumError whose Num is NOT an HTTP-date — e.g. some future
			// ParseInt of a non-date field. The extra HTTP-date narrowing rejects it.
			name: "ParseInt-shaped NumError whose Num is not an HTTP-date",
			err:  &strconv.NumError{Func: "ParseInt", Num: "not-a-date", Err: strconv.ErrSyntax},
			want: false,
		},
		{
			name: "non-NumError transport error",
			err:  errors.New("transport boom"),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHTTPDateRetryAfter429(tc.err); got != tc.want {
				t.Errorf("isHTTPDateRetryAfter429(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestSlackNotifierFallback429RespectsContextCancel ensures the fallback 429
// backoff is cancellable: a cancelled context must abort the wait promptly
// rather than sleeping out the default backoff.
func TestSlackNotifierFallback429RespectsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // always 429, no Retry-After
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	n := NewSlackNotifier("s", srv.URL, srv.Client())

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := n.Notify(ctx, Alert{Site: "s", ChangeType: "title", Severity: model.SeverityWarning, DetectedAt: time.Now()})
	if err == nil {
		t.Fatal("expected an error when context is cancelled mid-backoff")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("backoff did not honor context cancel promptly: took %s", elapsed)
	}
}

func TestSlackNotifierName(t *testing.T) {
	n := NewSlackNotifier("slack-digest", "https://hooks.slack.com/x", http.DefaultClient)
	if n.Name() != "slack-digest" {
		t.Errorf("Name() = %q, want slack-digest", n.Name())
	}
}
