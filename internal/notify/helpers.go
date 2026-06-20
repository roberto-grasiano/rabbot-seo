package notify

import (
	"io"
	"net/http"
	"strings"
)

// maxWebhookDrainBytes caps how much of a webhook response body we drain. The
// body is never used (only the status code matters), so we read a small bounded
// amount purely to let the keep-alive connection be reused; a hostile or
// compromised endpoint that returns a multi-gigabyte body on every alert (and
// every retry/redirect hop) cannot burn egress/CPU draining it into io.Discard.
const maxWebhookDrainBytes = 4 << 10 // 4 KiB

// drainClose drains and closes an HTTP response body so the underlying
// connection can be reused, then closed. Bodies left unread+unclosed leak the
// keep-alive connection (golangci-lint bodyclose flags the unclosed case). The
// drain is bounded by maxWebhookDrainBytes so a hostile endpoint cannot cause a
// bandwidth/CPU DoS by trickling an unbounded body within the client timeout.
func drainClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxWebhookDrainBytes))
	_ = resp.Body.Close()
}

// noRedirectClient returns a shallow copy of client that REFUSES every redirect:
// CheckRedirect returns http.ErrUseLastResponse so Do() hands back the 3xx itself
// (no error, body intact) and the caller treats it as a terminal delivery
// failure. A generic-webhook endpoint that 30x's is misconfigured, and following
// the Location would be an SSRF + credential/data-exfiltration vector — an
// attacker-controlled 302 could reach an internal/metadata host, and the stdlib
// would forward the configured auth headers (every header except the
// Authorization/Cookie/WWW-Authenticate trio on a cross-host hop) and the POST
// body to the target. We copy rather than mutate because BuildAlertingStack
// shares ONE *http.Client across the Slack and webhook notifiers; mutating
// CheckRedirect in place would change the Slack notifier's behavior too. Timeout
// and Transport are inherited so the configured timeout/SSRF-dial posture is kept.
func noRedirectClient(client *http.Client) *http.Client {
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &c
}

// replaceAllNonEmpty replaces every occurrence of old with new in s, but is a
// no-op when old is empty (strings.ReplaceAll with an empty old string would
// splice new between every rune — a footgun when scrubbing an absent secret).
func replaceAllNonEmpty(s, old, new string) string {
	if old == "" {
		return s
	}
	return strings.ReplaceAll(s, old, new)
}
