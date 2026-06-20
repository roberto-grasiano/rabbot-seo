package verify

import (
	"bytes"
	"context"
	"crypto/subtle"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
)

// httpTimeout bounds the whole verifier fetch. Generous enough for a slow
// origin, short enough that an inline wizard call cannot hang.
const httpTimeout = 15 * time.Second

// wellKnownPath is the fixed proof path. The token must sit here, on the literal
// host, returning 200 — no redirect is followed.
const wellKnownPath = "/.well-known/rabbot-verify.txt"

// wellKnownMaxBody caps the .well-known file read; a token is tiny.
const wellKnownMaxBody = 64 << 10 // 64 KiB

// metaMaxBody caps the homepage read for meta-tag parsing (matches doctor's
// MaxBodyBytes of 1 MiB).
const metaMaxBody = 1 << 20 // 1 MiB

// baseURL returns the scheme+host prefix the verifier fetches against. In
// production this is https://<host>; tests set Options.BaseOverride to a loopback
// httptest base so the same code path exercises the real client.
func baseURL(host string, opts Options) string {
	if opts.BaseOverride != "" {
		return strings.TrimRight(opts.BaseOverride, "/")
	}
	return "https://" + host
}

// fetchExact GETs url with the SSRF-guarded, NO-redirect client and returns the
// status and the response body (capped at maxBody). A non-200 is returned with
// its status so callers can treat a 404/30x as a non-fatal proof miss, not an
// error.
//
// Security (spec §Security): "a redirect to an attacker-controlled host must not
// satisfy a proof." The client refuses every redirect (CheckRedirect ==
// http.ErrUseLastResponse), so a 30x to an attacker host yields that 3xx status
// here, never the attacker's body. This guarantee is pinned by the load-bearing
// tests in redirect_security_test.go; if they fail, the verifier is following
// redirects — fix this client, never the test.
func fetchExact(ctx context.Context, url string, maxBody int64, allowPrivate bool) (status int, body []byte, err error) {
	client := fetcher.GuardedNoRedirectClient(httpTimeout, allowPrivate)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

// VerifyWellKnown checks the well-known file proof at the EXACT path on the
// literal host. A refused redirect (3xx) is ReasonRedirected; a 404/other non-200
// or an empty 200 body is ReasonNotFound; a 200 body that does not equal the
// token is ReasonMismatch; an equal body is ReasonVerified. The no-redirect,
// SSRF-guarded client is unchanged — a redirect to an attacker host still yields
// its 3xx here, never the attacker's body (redirect_security_test.go).
func VerifyWellKnown(ctx context.Context, host, token string, opts Options) (Reason, error) {
	if token == "" {
		return ReasonNotFound, nil
	}
	status, body, err := fetchExact(ctx, baseURL(host, opts)+wellKnownPath, wellKnownMaxBody, opts.AllowPrivate)
	if err != nil {
		return ReasonUnreachable, err
	}
	if status >= 300 && status < 400 {
		return ReasonRedirected, nil
	}
	if status != http.StatusOK {
		return ReasonNotFound, nil
	}
	got := strings.TrimSpace(string(body))
	if got == "" {
		return ReasonNotFound, nil
	}
	if tokenEqual(got, token) {
		return ReasonVerified, nil
	}
	return ReasonMismatch, nil
}

// VerifyMeta checks the homepage <meta name="rabbot-verify" content="<token>">
// proof. 3xx ⇒ ReasonRedirected; non-200 ⇒ ReasonNotFound; a present rabbot-
// verify meta whose content matches ⇒ ReasonVerified; present but none match ⇒
// ReasonMismatch; absent ⇒ ReasonNotFound. Same no-redirect SSRF-guarded client.
func VerifyMeta(ctx context.Context, host, token string, opts Options) (Reason, error) {
	if token == "" {
		return ReasonNotFound, nil
	}
	status, body, err := fetchExact(ctx, baseURL(host, opts)+"/", metaMaxBody, opts.AllowPrivate)
	if err != nil {
		return ReasonUnreachable, err
	}
	if status >= 300 && status < 400 {
		return ReasonRedirected, nil
	}
	if status != http.StatusOK {
		return ReasonNotFound, nil
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return ReasonUnreachable, err
	}
	present, matched := false, false
	doc.Find(`meta[name="rabbot-verify"]`).EachWithBreak(func(_ int, s *goquery.Selection) bool {
		content, ok := s.Attr("content")
		if !ok {
			return true
		}
		present = true
		if tokenEqual(strings.TrimSpace(content), token) {
			matched = true
			return false
		}
		return true
	})
	switch {
	case matched:
		return ReasonVerified, nil
	case present:
		return ReasonMismatch, nil
	default:
		return ReasonNotFound, nil
	}
}

// tokenEqual is a length-checked constant-time string compare.
func tokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
