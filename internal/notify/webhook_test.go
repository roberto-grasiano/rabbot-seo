package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// mustWebhook builds a webhook notifier, failing the test on a construction
// error. Used by the happy-path tests where the URL is always valid.
func mustWebhook(t *testing.T, name, url string, headers map[string]string, client *http.Client) Notifier {
	t.Helper()
	n, err := NewWebhookNotifier(name, url, headers, client)
	if err != nil {
		t.Fatalf("NewWebhookNotifier(%q): %v", name, err)
	}
	return n
}

// sampleAlert is a fully-populated grouped alert used to assert the wire DTO.
func sampleAlert() Alert {
	return Alert{
		Site:         "example.com",
		URL:          "https://example.com/p",
		ChangeType:   "title",
		Severity:     model.SeverityWarning,
		Before:       "Old Title",
		After:        "New Title",
		DetectedAt:   time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		GroupKey:     "example.com|title",
		RelatedCount: 3,
		DeepLink:     "https://example.com/p",
		Operational:  false,
		Items: []AlertItem{
			{URL: "https://example.com/a", Before: "a-old", After: "a-new"},
			{URL: "https://example.com/b", Before: "b-old", After: "b-new"},
		},
	}
}

// TestWebhookNotifierPostsVersionedJSON pins acceptance #5: the webhook notifier
// POSTs a stable, versioned snake_case JSON payload with Content-Type
// application/json and the configured static headers present.
func TestWebhookNotifierPostsVersionedJSON(t *testing.T) {
	var (
		mu        sync.Mutex
		gotMethod string
		gotCT     string
		gotAuth   string
		gotBody   []byte
		hits      int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		gotBody = body
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := mustWebhook(t, "glue", srv.URL,
		map[string]string{"Authorization": "Bearer s3cr3t-token"}, srv.Client())
	if err := n.Notify(context.Background(), sampleAlert()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected exactly 1 POST, got %d", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotAuth != "Bearer s3cr3t-token" {
		t.Errorf("Authorization header = %q, want the configured static value", gotAuth)
	}

	// Decode into a generic map so we assert on the exact snake_case wire keys,
	// not on Go field names — this is the public contract glue authors build against.
	var wire map[string]any
	if err := json.Unmarshal(gotBody, &wire); err != nil {
		t.Fatalf("payload is not valid JSON: %v\nbody: %s", err, gotBody)
	}
	wantKeys := []string{
		"payload_version", "site", "url", "change_type", "severity",
		"before", "after", "detected_at", "group_key", "related_count",
		"deep_link", "operational", "items",
	}
	for _, k := range wantKeys {
		if _, ok := wire[k]; !ok {
			t.Errorf("payload missing snake_case key %q; body: %s", k, gotBody)
		}
	}
	if pv, _ := wire["payload_version"].(float64); pv != 1 {
		t.Errorf("payload_version = %v, want 1", wire["payload_version"])
	}
	if wire["site"] != "example.com" {
		t.Errorf("site = %v, want example.com", wire["site"])
	}
	if wire["change_type"] != "title" {
		t.Errorf("change_type = %v, want title", wire["change_type"])
	}
	if wire["severity"] != "warning" {
		t.Errorf("severity = %v, want warning", wire["severity"])
	}
	if wire["detected_at"] != "2026-06-10T12:00:00Z" {
		t.Errorf("detected_at = %v, want RFC3339 UTC", wire["detected_at"])
	}
	if rc, _ := wire["related_count"].(float64); rc != 3 {
		t.Errorf("related_count = %v, want 3", wire["related_count"])
	}
	if op, _ := wire["operational"].(bool); op {
		t.Errorf("operational = %v, want false", wire["operational"])
	}
	items, ok := wire["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %v, want a 2-element array", wire["items"])
	}
	first, _ := items[0].(map[string]any)
	for _, k := range []string{"url", "before", "after"} {
		if _, ok := first[k]; !ok {
			t.Errorf("items[0] missing snake_case key %q; got %v", k, first)
		}
	}
}

// TestWebhookNotifierRetryPolicy pins acceptance #6: 429-with-Retry-After then
// 200 ⇒ exactly 2 attempts; 5xx retried up to the cap; 4xx terminal after 1
// attempt; ctx cancellation aborts a pending backoff promptly.
func TestWebhookNotifierRetryPolicy(t *testing.T) {
	t.Run("429 with Retry-After then 200 = 2 attempts", func(t *testing.T) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&hits, 1) == 1 {
				w.Header().Set("Retry-After", "0") // 0s so the test stays fast
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		n := mustWebhook(t, "w", srv.URL, nil, srv.Client())
		if err := n.Notify(context.Background(), sampleAlert()); err != nil {
			t.Fatalf("Notify: %v", err)
		}
		if got := atomic.LoadInt32(&hits); got != 2 {
			t.Errorf("expected exactly 2 attempts, got %d", got)
		}
	})

	t.Run("500 retried up to the cap then fails", func(t *testing.T) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.Header().Set("Retry-After", "0") // 0s so the cap is exercised fast
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		n := mustWebhook(t, "w", srv.URL, nil, srv.Client())
		err := n.Notify(context.Background(), sampleAlert())
		if err == nil {
			t.Fatal("expected an error after exhausting retries on 500")
		}
		// 1 initial attempt + maxWebhookRetries retries.
		if got := atomic.LoadInt32(&hits); got != int32(maxWebhookRetries+1) {
			t.Errorf("expected %d attempts (1 + %d retries), got %d", maxWebhookRetries+1, maxWebhookRetries, got)
		}
	})

	t.Run("no trailing backoff after the final failed attempt", func(t *testing.T) {
		// Every attempt is a 5xx with a large Retry-After. The loop must back off
		// BETWEEN attempts only — never AFTER the last attempt (there is no subsequent
		// request to wait for). We count backoff waits via an injected sleep seam: with
		// 1 initial try + maxWebhookRetries retries there are exactly maxWebhookRetries
		// inter-attempt gaps, so the loop must sleep exactly maxWebhookRetries times,
		// not maxWebhookRetries+1. Each requested wait must also be the honest
		// (capped) Retry-After, proving the value, not just the count.
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.Header().Set("Retry-After", "30") // a real cool-down a trailing sleep would block on
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		var waits []time.Duration
		n := mustWebhook(t, "w", srv.URL, nil, srv.Client())
		wn := n.(*webhookNotifier)
		// Replace the real backoff with an instant, recording one so the test is fast
		// and deterministic (no wall-clock dependence) yet still observes ctx.
		wn.sleep = func(ctx context.Context, d time.Duration) error {
			waits = append(waits, d)
			return ctx.Err()
		}

		err := n.Notify(context.Background(), sampleAlert())
		if err == nil {
			t.Fatal("expected an error after exhausting retries on 503")
		}
		if got := atomic.LoadInt32(&hits); got != int32(maxWebhookRetries+1) {
			t.Errorf("expected %d attempts, got %d", maxWebhookRetries+1, got)
		}
		if len(waits) != maxWebhookRetries {
			t.Errorf("expected exactly %d inter-attempt backoffs (no trailing sleep after the final attempt), got %d: %v",
				maxWebhookRetries, len(waits), waits)
		}
		// 30s requested is below maxRetryAfter, so the honest wait is 30s (uncapped).
		const wantWait = 30 * time.Second
		for i, w := range waits {
			if w != wantWait {
				t.Errorf("backoff[%d] = %s, want the requested Retry-After %s", i, w, wantWait)
			}
		}
	})

	t.Run("400 terminal after 1 attempt", func(t *testing.T) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.WriteHeader(http.StatusBadRequest)
		}))
		defer srv.Close()

		n := mustWebhook(t, "w", srv.URL, nil, srv.Client())
		err := n.Notify(context.Background(), sampleAlert())
		if err == nil {
			t.Fatal("expected an error for a 400")
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Errorf("4xx must be terminal: expected 1 attempt, got %d", got)
		}
	})

	t.Run("404 terminal after 1 attempt", func(t *testing.T) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		n := mustWebhook(t, "w", srv.URL, nil, srv.Client())
		if err := n.Notify(context.Background(), sampleAlert()); err == nil {
			t.Fatal("expected an error for a 404")
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Errorf("4xx must be terminal: expected 1 attempt, got %d", got)
		}
	})

	t.Run("ctx cancellation aborts a pending backoff promptly", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "120") // far beyond test runtime
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		n := mustWebhook(t, "w", srv.URL, nil, srv.Client())
		start := time.Now()
		err := n.Notify(ctx, sampleAlert())
		if err == nil {
			t.Fatal("expected an error when ctx is cancelled mid-backoff")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("backoff did not honor ctx cancel promptly: took %s", elapsed)
		}
	})
}

// TestWebhookErrorScrubsURL pins acceptance #7: a transport error's Error() must
// not contain the webhook URL (path/query/secrets), mirroring the slack redaction.
// The host may appear (it is operational context, not the secret); the path and
// query — which may carry a token — must not.
func TestWebhookErrorScrubsURL(t *testing.T) {
	// A URL whose path is the secret (e.g. an ntfy/Slack-style token-in-path).
	const secretPath = "SUPERSECRET-TOKEN-IN-PATH"
	webhookURL := "http://127.0.0.1:1/" + secretPath + "?token=ALSO-SECRET"

	client := &http.Client{Transport: errRoundTripper{}}
	n := mustWebhook(t, "glue", webhookURL, nil, client)

	err := n.Notify(context.Background(), sampleAlert())
	if err == nil {
		t.Fatal("expected an error from a failing transport, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, secretPath) {
		t.Errorf("error leaked the URL path secret %q: %s", secretPath, msg)
	}
	if strings.Contains(msg, "ALSO-SECRET") {
		t.Errorf("error leaked the URL query secret: %s", msg)
	}
	if strings.Contains(msg, webhookURL) {
		t.Errorf("error leaked the full webhook URL: %s", msg)
	}
}

// TestWebhookErrorNeverContainsHeaderValues guards the secret-hygiene rule for
// the static auth headers: a failed send must never echo a header VALUE (e.g. a
// bearer token) into the returned error.
func TestWebhookErrorNeverContainsHeaderValues(t *testing.T) {
	const bearer = "Bearer SECRET-HEADER-VALUE-XYZ"
	client := &http.Client{Transport: errRoundTripper{}}
	n := mustWebhook(t, "glue", "http://127.0.0.1:1/hook",
		map[string]string{"Authorization": bearer}, client)

	err := n.Notify(context.Background(), sampleAlert())
	if err == nil {
		t.Fatal("expected a transport error, got nil")
	}
	if strings.Contains(err.Error(), "SECRET-HEADER-VALUE-XYZ") {
		t.Errorf("error leaked a static auth header value: %v", err)
	}

	// Load-bearing-scrub guard: a transport error does not itself echo the header,
	// so prove scrub() removes a header value from an error that DOES contain it.
	wn, ok := n.(*webhookNotifier)
	if !ok {
		t.Fatalf("expected *webhookNotifier, got %T", n)
	}
	leaky := fmt.Errorf("dial failed sending header Authorization: %s", bearer)
	if scrubbed := wn.scrub(leaky); strings.Contains(scrubbed.Error(), "SECRET-HEADER-VALUE-XYZ") {
		t.Errorf("scrub() failed to redact a header value: %v", scrubbed)
	}
}

// countingBody is a ReadCloser that records how many bytes were read from it, so
// a test can prove the webhook notifier does NOT drain an unbounded response body.
type countingBody struct {
	read int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	// Always have more to give; the notifier should stop well before this runs away.
	for i := range p {
		p[i] = 'x'
	}
	atomic.AddInt64(&c.read, int64(len(p)))
	return len(p), nil
}

func (c *countingBody) Close() error { return nil }

// bigBodyRoundTripper returns a 200 whose body is an effectively infinite stream,
// to prove drainClose bounds the read.
type bigBodyRoundTripper struct{ body *countingBody }

func (rt bigBodyRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       rt.body,
		Header:     make(http.Header),
	}, nil
}

// TestWebhookNotifierBoundsResponseDrain pins the DoS hardening: the response body
// is never used (only the status code matters), so a hostile/compromised endpoint
// that returns a multi-gigabyte body must NOT be drained in full. The notifier
// reads at most a small bounded amount before closing the connection.
func TestWebhookNotifierBoundsResponseDrain(t *testing.T) {
	body := &countingBody{}
	client := &http.Client{Transport: bigBodyRoundTripper{body: body}}
	n := mustWebhook(t, "glue", "http://example.com/hook", nil, client)

	if err := n.Notify(context.Background(), sampleAlert()); err != nil {
		t.Fatalf("Notify against a 200 should succeed: %v", err)
	}
	// The drain is bounded to a few KiB; allow generous slack for io.Copy's internal
	// buffering but assert it is nowhere near "unbounded" (a real attack streams GBs).
	if got := atomic.LoadInt64(&body.read); got > 1<<20 {
		t.Errorf("response drain not bounded: read %d bytes (expected a small bounded amount)", got)
	}
}

// TestWebhookNotifierRefusesRedirects pins the SSRF / credential-exfiltration
// hardening: a generic-webhook endpoint that answers with a 3xx is MISCONFIGURED,
// and the notifier must NOT follow the Location to a second host. A followed
// redirect would (a) let an attacker-controlled 302 reach an internal/metadata
// address and (b) forward the configured auth headers AND the alert body (which
// carries crawled Before/After content) to the redirect target. The notifier must
// treat the 3xx as a terminal delivery failure: exactly one request reaches the
// first host, the second host is never dialed, and Notify returns an error.
func TestWebhookNotifierRefusesRedirects(t *testing.T) {
	for _, code := range []int{
		http.StatusMovedPermanently,  // 301
		http.StatusFound,             // 302
		http.StatusTemporaryRedirect, // 307 (preserves method + body)
		http.StatusPermanentRedirect, // 308
	} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var (
				secondHits int32
				gotAPIKey  string
				gotAuthz   string
				gotBodyLen int64
			)
			// The redirect TARGET stands in for an attacker-controlled host (e.g. a
			// metadata endpoint). It records whether it was reached and whether any
			// secret header or the alert body was forwarded to it.
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&secondHits, 1)
				gotAPIKey = r.Header.Get("X-Api-Key")
				gotAuthz = r.Header.Get("Authorization")
				gotBodyLen = r.ContentLength
				w.WriteHeader(http.StatusOK)
			}))
			defer second.Close()

			var firstHits int32
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&firstHits, 1)
				http.Redirect(w, r, second.URL+"/exfil", code)
			}))
			defer first.Close()

			n := mustWebhook(t, "glue", first.URL, map[string]string{
				"X-API-Key":     "CUSTOM-AUTH-SECRET-XYZ",
				"Authorization": "Bearer BEARER-SECRET-XYZ",
			}, first.Client())

			err := n.Notify(context.Background(), sampleAlert())
			if err == nil {
				t.Fatalf("a webhook endpoint that returns %d must be a delivery failure, got nil", code)
			}
			if got := atomic.LoadInt32(&firstHits); got != 1 {
				t.Errorf("first host should be hit exactly once, got %d", got)
			}
			if got := atomic.LoadInt32(&secondHits); got != 0 {
				t.Errorf("redirect must NOT be followed: second (attacker) host was dialed %d times", got)
			}
			if gotAPIKey != "" {
				t.Errorf("custom auth header leaked to the redirect target: %q", gotAPIKey)
			}
			if gotAuthz != "" {
				t.Errorf("Authorization header leaked to the redirect target: %q", gotAuthz)
			}
			if gotBodyLen > 0 {
				t.Errorf("alert body (%d bytes) was forwarded to the redirect target", gotBodyLen)
			}
		})
	}
}

// TestWebhookRedirectPolicyDoesNotMutateSharedClient guards that installing the
// no-redirect policy on the webhook's client never mutates a client the caller
// shares with other notifiers (BuildAlertingStack hands the SAME *http.Client to
// the Slack and webhook notifiers). The injected client's CheckRedirect must be
// untouched after construction.
func TestWebhookRedirectPolicyDoesNotMutateSharedClient(t *testing.T) {
	shared := &http.Client{Timeout: 30 * time.Second}
	if shared.CheckRedirect != nil {
		t.Fatal("precondition: shared client should start with a nil CheckRedirect")
	}
	_ = mustWebhook(t, "glue", "http://example.com/hook", nil, shared)
	if shared.CheckRedirect != nil {
		t.Error("NewWebhookNotifier mutated the caller-shared client's CheckRedirect")
	}
}

func TestWebhookNotifierName(t *testing.T) {
	n := mustWebhook(t, "glue", "http://example.com/hook", nil, http.DefaultClient)
	if n.Name() != "glue" {
		t.Errorf("Name() = %q, want glue", n.Name())
	}
}

// TestWebhookNotifierRejectsEmptyURL pins the webhook half of acceptance #8: a
// missing url is a construction error naming the notifier (so daemon startup
// fails, not the first send) and echoes no header value.
func TestWebhookNotifierRejectsEmptyURL(t *testing.T) {
	_, err := NewWebhookNotifier("glue", "",
		map[string]string{"Authorization": "Bearer SECRET-XYZ"}, http.DefaultClient)
	if err == nil {
		t.Fatal("expected an error for an empty webhook url")
	}
	if !strings.Contains(err.Error(), "glue") {
		t.Errorf("error should name the notifier: %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-XYZ") {
		t.Errorf("construction error leaked a header value: %v", err)
	}
}
