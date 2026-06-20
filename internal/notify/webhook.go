package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// maxWebhookRetries is the number of RETRIES after the initial attempt, so the
// send loop runs up to maxWebhookRetries+1 times. It mirrors the Slack policy
// shape (maxSlackRetries) so both backends behave consistently under load.
const maxWebhookRetries = 3

// webhookPayloadVersion is the schema version of the wire DTO. It is the first
// field of every payload so glue authors can branch on it; bump it (never reuse)
// on any breaking change to the wire shape.
const webhookPayloadVersion = 1

// webhookPayload is the stable, versioned, snake_case JSON contract POSTed to a
// generic-webhook endpoint. It is intentionally a DISTINCT type from notify.Alert
// (which carries Go-internal field names and an evolving shape): the wire format
// is decoupled from the internal struct and versioned via PayloadVersion so glue
// built against it does not break when Alert changes.
type webhookPayload struct {
	PayloadVersion int           `json:"payload_version"`
	Site           string        `json:"site"`
	URL            string        `json:"url"`
	ChangeType     string        `json:"change_type"`
	Severity       string        `json:"severity"`
	Before         string        `json:"before"`
	After          string        `json:"after"`
	DetectedAt     string        `json:"detected_at"` // RFC3339 UTC
	GroupKey       string        `json:"group_key"`
	RelatedCount   int           `json:"related_count"`
	DeepLink       string        `json:"deep_link"`
	Operational    bool          `json:"operational"`
	Items          []webhookItem `json:"items"`
}

// webhookItem is one rolled-up affected page in the wire payload.
type webhookItem struct {
	URL    string `json:"url"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// toWebhookPayload renders an Alert into its versioned wire DTO.
func toWebhookPayload(a Alert) webhookPayload {
	items := make([]webhookItem, 0, len(a.Items))
	for _, it := range a.Items {
		// AlertItem and webhookItem are deliberately distinct types (internal vs
		// versioned wire shape) but share field names/types, so a direct conversion
		// is valid and keeps the wire DTO independent of the internal struct.
		items = append(items, webhookItem(it))
	}
	return webhookPayload{
		PayloadVersion: webhookPayloadVersion,
		Site:           a.Site,
		URL:            a.URL,
		ChangeType:     a.ChangeType,
		Severity:       string(a.Severity),
		Before:         a.Before,
		After:          a.After,
		DetectedAt:     a.DetectedAt.UTC().Format(time.RFC3339),
		GroupKey:       a.GroupKey,
		RelatedCount:   a.RelatedCount,
		DeepLink:       a.DeepLink,
		Operational:    a.Operational,
		Items:          items,
	}
}

// webhookNotifier POSTs the versioned JSON payload to one operator URL with
// optional static request headers, retrying on 429/5xx (Slack-policy shape).
type webhookNotifier struct {
	name    string
	url     string
	headers map[string]string
	client  *http.Client
	// sleep performs the inter-attempt backoff, returning ctx.Err() if ctx is
	// cancelled during the wait. It is a seam: production uses sleepCtx (a real,
	// ctx-cancellable timer); tests inject a recording/instant stub. nil ⇒ sleepCtx.
	sleep func(ctx context.Context, d time.Duration) error
}

// NewWebhookNotifier builds a generic-webhook notifier. headers are sent verbatim
// on every request (covering `Authorization: Bearer …` style auth) and are NEVER
// logged or echoed into errors. A nil client defaults to a 30s-timeout client
// (never http.DefaultClient, which has no timeout) — matching the Slack backend.
//
// An empty webhookURL is rejected HERE, at construction, so a misconfigured
// notifier fails daemon startup (naming the notifier) rather than at first send.
// The error never echoes a header value. The (Notifier, error) signature mirrors
// NewEmailNotifier so the supervisor wiring validates both new types uniformly.
func NewWebhookNotifier(name, webhookURL string, headers map[string]string, client *http.Client) (Notifier, error) {
	if webhookURL == "" {
		return nil, fmt.Errorf("generic-webhook %q: incomplete config, missing url", name)
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	client = noRedirectClient(client)
	// Copy headers so a later mutation of the caller's map can't change what we send.
	var hdr map[string]string
	if len(headers) > 0 {
		hdr = make(map[string]string, len(headers))
		for k, v := range headers {
			hdr[k] = v
		}
	}
	return &webhookNotifier{name: name, url: webhookURL, headers: hdr, client: client}, nil
}

func (w *webhookNotifier) Name() string { return w.name }

// Notify POSTs the alert as versioned JSON, retrying on 429 (honoring an
// integer-seconds Retry-After, capped at maxRetryAfter) and on 5xx up to
// maxWebhookRetries; other 4xx are terminal after one attempt. Transport and
// HTTP errors are scrubbed so the returned error never carries the URL path,
// query, or any header value.
func (w *webhookNotifier) Notify(ctx context.Context, a Alert) error {
	body, err := json.Marshal(toWebhookPayload(a))
	if err != nil {
		// Marshalling a fixed-shape DTO of strings/ints never fails in practice;
		// surface a scrubbed error rather than the raw one out of caution.
		return fmt.Errorf("generic-webhook %q: marshal payload", w.name)
	}

	var lastErr error
	for attempt := 0; attempt <= maxWebhookRetries; attempt++ {
		retry, wait, err := w.attempt(ctx, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry {
			break
		}
		// Back off ONLY between attempts. On the final iteration there is no
		// subsequent request, so a backoff here would just block (up to maxRetryAfter
		// — a real 503 Retry-After can be ~60s) with nothing to wait for. Break first.
		if attempt == maxWebhookRetries {
			break
		}
		if wait > maxRetryAfter {
			wait = maxRetryAfter
		}
		if serr := w.backoff(ctx, wait); serr != nil {
			return serr
		}
	}
	return w.scrub(lastErr)
}

// backoff waits d before the next attempt, honoring ctx via the sleep seam
// (production: sleepCtx). It returns ctx.Err() if ctx is cancelled during the wait
// so the caller aborts promptly instead of finishing the sleep.
func (w *webhookNotifier) backoff(ctx context.Context, d time.Duration) error {
	if w.sleep != nil {
		return w.sleep(ctx, d)
	}
	return sleepCtx(ctx, d)
}

// sleepCtx sleeps for d but returns early with ctx.Err() if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// attempt performs one POST. It returns (retry, wait, err): err==nil means
// success (2xx); retry reports whether the caller should back off and try again;
// wait is the backoff for that retry. The *http.Response body is always drained
// and closed so the connection can be reused.
func (w *webhookNotifier) attempt(ctx context.Context, body []byte) (retry bool, wait time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return false, 0, err // a malformed URL is terminal, not retryable
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		// Transport error (connection refused, timeout, DNS). Terminal — do NOT
		// hammer a dead endpoint, mirroring the Slack backend which breaks on any
		// non-rate-limit error. The next real change alert will try again.
		return false, 0, err
	}
	defer drainClose(resp)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, 0, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return true, retryAfter(resp), fmt.Errorf("generic-webhook %q: HTTP %d", w.name, resp.StatusCode)
	case resp.StatusCode >= 500:
		// 5xx is retryable; honor a Retry-After if the server sent one (e.g. a 503
		// with a cool-down), else use the default backoff.
		return true, retryAfter(resp), fmt.Errorf("generic-webhook %q: HTTP %d", w.name, resp.StatusCode)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Redirects are REFUSED by construction (noRedirectClient sets CheckRedirect
		// to http.ErrUseLastResponse), so a 3xx surfaces here as the final response
		// rather than being silently followed. A generic-webhook endpoint that 30x's
		// is misconfigured (and following it would be an SSRF / secret-exfiltration
		// vector); treat it as a terminal delivery failure.
		return false, 0, fmt.Errorf("generic-webhook %q: refusing to follow redirect (HTTP %d); point the webhook at the final URL", w.name, resp.StatusCode)
	default:
		// Other 4xx are terminal.
		return false, 0, fmt.Errorf("generic-webhook %q: HTTP %d", w.name, resp.StatusCode)
	}
}

// retryAfter reads an integer-seconds Retry-After header, falling back to
// defaultRetryAfter when absent or non-integer (RFC 7231 also permits an
// HTTP-date, which we do not parse — the default backoff still fires).
func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return defaultRetryAfter
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return defaultRetryAfter
}

// scrub strips secrets from a transport/HTTP error before it leaves the package.
// The webhook URL's path and query may carry a token (ntfy/Slack-style), and the
// static auth headers are secrets too; neither may appear in logs, the
// notify-test control response, or terminal scrollback (CLAUDE.md hard rule).
// The host:port is kept as operational context — it is not the secret and helps
// an operator diagnose a wrong endpoint. Returning a fresh fmt.Errorf with %s
// (not %w) severs the *url.Error chain so no downstream Unwrap can recover .URL.
func (w *webhookNotifier) scrub(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// Replace the full URL and the path-and-query (the parts that may be secret),
	// leaving the scheme://host:port visible. url.Parse failures fall back to
	// scrubbing the whole configured URL string.
	if u, perr := url.Parse(w.url); perr == nil && u.Host != "" {
		full := u.String()
		hostOnly := u.Scheme + "://" + u.Host
		msg = replaceAllNonEmpty(msg, full, hostOnly+"/<redacted-path>")
		if rq := u.RequestURI(); rq != "" && rq != "/" {
			msg = replaceAllNonEmpty(msg, rq, "/<redacted-path>")
		}
	} else {
		msg = replaceAllNonEmpty(msg, w.url, "<redacted-webhook-url>")
	}
	for _, hv := range w.headers {
		msg = replaceAllNonEmpty(msg, hv, "<redacted>")
	}
	return fmt.Errorf("generic-webhook %q: %s", w.name, msg)
}
