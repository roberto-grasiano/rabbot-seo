package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// maxSlackRetries is the number of RETRIES after the initial attempt, so the
// send loop runs up to maxSlackRetries+1 times (1 initial try + 3 retries).
const maxSlackRetries = 3

// maxRetryAfter caps how long a single 429 Retry-After is honored. The wait is
// taken from the response header (endpoint-controlled) and is served while a
// frontier per-host slot is held, so an unbounded value could pin a crawl worker;
// cap it so a misbehaving endpoint cannot stall the crawler.
const maxRetryAfter = 60 * time.Second

// defaultRetryAfter is the backoff used when Slack returns a 429 but slack-go
// could NOT produce a typed *RateLimitedError — i.e. the 429 either omitted the
// Retry-After header or used an RFC 7231 HTTP-date (slack-go only parses
// integer-seconds). Without this fallback those 429 variants would be treated as
// non-retryable and the alert would be dropped after a single attempt.
const defaultRetryAfter = 2 * time.Second

// slackNotifier posts Block Kit messages to one channel-locked Incoming Webhook,
// serializing sends (~1 msg/sec) and honoring 429 Retry-After.
type slackNotifier struct {
	name   string
	url    string
	client *http.Client
	send   chan struct{} // single-token coalescing gate (~1 msg/sec)
}

// NewSlackNotifier builds a Slack Incoming-Webhook notifier. A nil client
// defaults to an http.Client with a 30s timeout (never http.DefaultClient,
// which has no timeout).
func NewSlackNotifier(name, webhookURL string, client *http.Client) Notifier {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	gate := make(chan struct{}, 1)
	gate <- struct{}{} // one available token
	return &slackNotifier{name: name, url: webhookURL, client: client, send: gate}
}

func (s *slackNotifier) Name() string { return s.name }

// Notify renders the alert to Block Kit and posts it, honoring Slack's ~1 msg/sec
// pacing via a single-token gate and retrying on 429. The wait honors an
// integer-seconds Retry-After when present; a 429 that omits Retry-After or uses
// an HTTP-date falls back to defaultRetryAfter (both still retried), and the wait
// is capped at maxRetryAfter.
func (s *slackNotifier) Notify(ctx context.Context, a Alert) error {
	// Acquire the pacing token (released ~1s later).
	select {
	case <-s.send:
	case <-ctx.Done():
		return ctx.Err()
	}
	// Release the pacing token ~1s later (enforces ~1 msg/sec). This timer
	// goroutine is deliberately fire-and-forget — NOT registered with the daemon's
	// shutdown WaitGroup/drain — and that is safe by construction, not by accident:
	//   - it is ctx-cancellable: on ctx cancel (daemon shutdown) it stops waiting
	//     and returns the token immediately instead of sleeping the full second; and
	//   - its only post-wait action is a non-blocking send to the cap-1 gate, which
	//     is empty here (we hold the token), so the send can never block.
	// Its worst-case lifetime once shutdown begins is therefore a single
	// non-blocking channel send — nothing the drain would need to wait for. Joining
	// it to a WaitGroup would add cross-package shutdown plumbing for no observable
	// benefit; documenting the scope here is the reviewed decision (PR #21 review).
	defer func() {
		go func() {
			t := time.NewTimer(time.Second)
			defer t.Stop()
			select {
			case <-t.C:
			case <-ctx.Done():
			}
			s.send <- struct{}{}
		}()
	}()

	blocks := BuildBlocks(a)
	msg := &slack.WebhookMessage{Blocks: &blocks}

	var lastErr error
	for attempt := 0; attempt <= maxSlackRetries; attempt++ {
		err := slack.PostWebhookCustomHTTPContext(ctx, s.url, s.client, msg)
		if err == nil {
			return nil
		}
		lastErr = err

		// Determine the backoff for a 429. slack-go (pinned v0.24.0) only returns a
		// typed *slack.RateLimitedError when the 429 carries an integer-seconds
		// Retry-After header (checkStatusCode -> strconv.ParseInt). Two other real
		// 429 variants do NOT yield that type and would otherwise be treated as
		// non-retryable: (1) a 429 with NO Retry-After header surfaces as
		// slack.StatusCodeError{Code:429}; (2) a 429 with an RFC 7231 HTTP-date
		// Retry-After makes ParseInt fail and surfaces as a raw parse error. For
		// both we fall back to defaultRetryAfter so the backoff machinery actually
		// fires. Non-429 transport errors fall through to the break (don't hammer).
		wait, retry := time.Duration(0), false
		var rlErr *slack.RateLimitedError
		var scErr slack.StatusCodeError
		switch {
		case errors.As(err, &rlErr):
			wait, retry = rlErr.RetryAfter, true
		case errors.As(err, &scErr) && scErr.Code == http.StatusTooManyRequests:
			wait, retry = defaultRetryAfter, true
		case isHTTPDateRetryAfter429(err):
			wait, retry = defaultRetryAfter, true
		}
		if !retry {
			// Non-rate-limit error: don't hammer.
			break
		}
		if wait > maxRetryAfter {
			wait = maxRetryAfter
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			continue
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	// Never propagate the raw transport error: slack-go wraps the *url.Error,
	// whose Error() embeds the full webhook URL — which IS the secret for an
	// Incoming Webhook. Scrub it before returning so the URL cannot leak into the
	// notify-test control response, logs, or terminal scrollback (CLAUDE.md: Slack
	// alerts must NEVER log webhook URLs/tokens). Returning a string (not %w) also
	// severs the *url.Error chain so no downstream Unwrap can recover .URL.
	sanitized := lastErr.Error()
	if s.url != "" {
		sanitized = strings.ReplaceAll(sanitized, s.url, "<redacted-webhook-url>")
	}
	return fmt.Errorf("slack webhook %q: %s", s.name, sanitized)
}

// isHTTPDateRetryAfter429 reports whether err is the raw strconv parse failure
// that slack-go (v0.24.0 checkStatusCode) returns when a 429 carries a non-integer
// Retry-After header — RFC 7231 permits an HTTP-date, but slack-go only parses
// integer-seconds and returns the *strconv.NumError verbatim. slack-go parses that
// header via strconv.ParseInt (misc.go checkStatusCode), so the surfaced NumError
// has Func == "ParseInt" and Num set to the offending header value (the HTTP-date).
//
// We deliberately do NOT treat any *strconv.NumError as a 429: the heuristic is
// narrowed to (a) Func == "ParseInt" — the Retry-After parse path — AND (b) Num
// must itself parse as an RFC 7231 HTTP-date. Both must hold, so an unrelated
// integer-parse failure (e.g. an Atoi/ParseFloat NumError, or a ParseInt of some
// non-date field) is rejected rather than misclassified as a retryable 429.
//
// DEP-BUMP CAVEAT: this still couples to a slack-go internal — today the ONLY
// ParseInt that surfaces to the caller is that Retry-After header parse, and its
// argument is an HTTP-date. Re-verify against the slack-go source whenever the
// dependency is bumped: a new path that ParseInts an HTTP-date-shaped string and
// surfaces it would be the only way to fool the tightened check; an Atoi/ParseFloat
// path, or a ParseInt of a non-date string, no longer trips it. (Guarded by
// TestSlackNotifierRetriesOn429WithHTTPDateRetryAfter and
// TestIsHTTPDateRetryAfter429TightensAgainstUnrelatedNumError.)
func isHTTPDateRetryAfter429(err error) bool {
	var numErr *strconv.NumError
	if !errors.As(err, &numErr) {
		return false
	}
	// slack-go parses the Retry-After header via strconv.ParseInt; only that
	// function name is the Retry-After parse path.
	if numErr.Func != "ParseInt" {
		return false
	}
	// Extra narrowing: the offending value must be a valid RFC 7231 HTTP-date,
	// which is exactly what makes ParseInt fail in the real 429 case.
	_, parseErr := time.Parse(http.TimeFormat, strings.TrimSpace(numErr.Num))
	return parseErr == nil
}
