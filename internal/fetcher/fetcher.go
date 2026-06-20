// Package fetcher fetches URLs over net/http, capturing the full redirect chain,
// response timing, and headers, and classifies every response into a model.FetchClass
// (§5A). Bodies are only returned for ok-class fetches.
package fetcher

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// maxRedirects caps the redirect chain. Kept tight (was 20) so a hostile chain
// cannot waste many dials before the depth limit ends it.
const maxRedirects = 10

// Result carries everything downstream needs, including the FetchClass (§5A),
// redirect chain, timing, and response headers.
type Result struct {
	FinalURL      string
	HTTPStatus    int
	FetchClass    model.FetchClass
	StatusType    model.StatusType
	RedirectChain []string
	Header        http.Header
	Body          []byte
	// Truncated is set when the response body exceeded Options.MaxBodyBytes and was
	// cut to the cap. Downstream can record/skip-diff on a truncated snapshot rather
	// than treating the prefix as the complete page (§5A: silent truncation would
	// make a content change beyond the cutoff invisible).
	Truncated    bool
	NotModified  bool
	ResponseTime time.Duration
	FetchedAt    time.Time
	Detector     string
	Err          error
}

// Request includes per-site access settings (§5A) resolved from config.
type Request struct {
	URL       string
	ETag      string
	LastMod   string
	Headers   map[string]string
	BasicUser string
	BasicPass string
	Cookies   []*http.Cookie
	ProxyURL  string
}

type Fetcher interface {
	Fetch(ctx context.Context, req Request) (Result, error)
	// AllowsPrivate reports whether this fetcher's SSRF guard is disabled (private
	// /loopback/link-local/metadata destinations permitted). Callers that admit
	// sites (e.g. reconcile) consult it so their own URL validation mirrors the
	// fetcher: production rejects internal ranges, the test suite (AllowPrivate)
	// keeps targeting loopback httptest servers.
	AllowsPrivate() bool
}

// Options configures the shared HTTP client.
type Options struct {
	UserAgent string
	// UserAgentFunc, when non-nil, computes the per-request User-Agent from the
	// target host (httpReq.URL.Hostname()) instead of the static UserAgent. The
	// daemon uses it to send a per-site trust-signalling UA (verified-for /
	// contact-unverified / confirm-or-block) while crawling many hosts through one
	// shared fetcher. A nil func keeps the static UserAgent (back-compat).
	UserAgentFunc func(host string) string
	Timeout       time.Duration
	MaxBodyBytes  int64
	// AllowPrivate disables the SSRF guard, permitting dials/redirects to
	// loopback/private/link-local/metadata addresses. Off by default; only the
	// test suite (which targets 127.0.0.1) and explicit operator opt-in set it.
	AllowPrivate bool
}

type httpFetcher struct {
	opts   Options
	direct *http.Client
}

// New returns a Fetcher with a shared direct client. Per-request proxies build a
// dedicated transport on demand.
func New(opts Options) Fetcher {
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxBodyBytes == 0 {
		opts.MaxBodyBytes = 5 << 20
	}
	direct, _ := newClient(opts, "")
	return &httpFetcher{opts: opts, direct: direct}
}

// newClient builds an HTTP client. Unless opts.AllowPrivate is set, its dialer
// installs the SSRF guard via Control, which runs on every dial — including
// redirect dials — with the post-DNS IP, rejecting internal destinations. A
// non-empty but unparseable proxyURL is an error (fail-closed), never silently
// ignored into direct egress.
func newClient(opts Options, proxyURL string) (*http.Client, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if !opts.AllowPrivate {
		dialer.Control = dialControl
	}
	tr := &http.Transport{
		DialContext:         dialer.DialContext,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// Leave DisableCompression false so the transport adds Accept-Encoding: gzip
		// itself and transparently decompresses the response. Fetch must not set
		// Accept-Encoding manually, which would defeat that transparent decode.
		DisableCompression:    false,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	if proxyURL != "" {
		pu, err := url.Parse(proxyURL)
		if err != nil {
			return nil, errBadProxyURL
		}
		// Defense-in-depth: when the SSRF guard is active, reject a proxy host that
		// is a disallowed IP literal (cloud metadata / loopback / private /
		// link-local) at build time. The transport dials the proxy via the same
		// guarded DialContext, so such a dial would already be refused by
		// dialControl — but failing closed here yields a clear config-time error
		// instead of an opaque late dial failure, and survives any future
		// transport rewiring. AllowPrivate skips the guard, mirroring the dialer.
		if !opts.AllowPrivate {
			if ip := net.ParseIP(pu.Hostname()); ip != nil && ipDisallowed(ip) {
				return nil, errDisallowedDestination
			}
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	return &http.Client{Transport: tr, Timeout: opts.Timeout}, nil
}

// AllowsPrivate reports whether the SSRF guard is disabled for this fetcher.
func (f *httpFetcher) AllowsPrivate() bool { return f.opts.AllowPrivate }

func (f *httpFetcher) client(req Request) (*http.Client, error) {
	if req.ProxyURL != "" {
		return newClient(f.opts, req.ProxyURL)
	}
	return f.direct, nil
}

func (f *httpFetcher) Fetch(ctx context.Context, req Request) (Result, error) {
	res := Result{FetchedAt: time.Now()}
	chain := []string{req.URL}

	base, err := f.client(req)
	if err != nil {
		// e.g. a misconfigured per-site proxy_url: fail closed rather than
		// egressing directly from the daemon's real IP.
		res.Err = err
		res.FetchClass = model.FetchUnreachable
		res.StatusType = model.StatusUnreachable
		return res, nil
	}

	// Identify per-site credential headers so they can be stripped on
	// cross-host redirects (stdlib only strips Authorization/Cookie, not
	// arbitrary custom headers).
	credHeaders := credentialHeaderKeys(req.Headers)

	// redirectCapHit records that the chain exhausted maxRedirects (loop or an
	// over-long chain). ErrUseLastResponse makes Do() return the truncated final
	// 3xx with no error; without this flag that hop would be misclassified FetchOK
	// and its tiny redirect body extracted/snapshotted as real page content.
	var redirectCapHit bool

	// Wrap the base client to capture the redirect chain and re-validate every
	// redirect target against the SSRF deny-list.
	client := &http.Client{
		Transport: base.Transport,
		Timeout:   base.Timeout,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			chain = append(chain, r.URL.String())
			if len(via) >= maxRedirects {
				redirectCapHit = true
				return http.ErrUseLastResponse
			}
			// Abort the chain if the target is a disallowed scheme/host even
			// when AllowPrivate bypasses the dial-time guard for IP literals.
			if !f.opts.AllowPrivate && !redirectAllowed(r.URL) {
				return errDisallowedDestination
			}
			// On any cross-host redirect, strip per-site credential headers so
			// an attacker-controlled host cannot harvest the operator's API key.
			if len(via) > 0 && !sameHost(via[len(via)-1].URL, r.URL) {
				for _, k := range credHeaders {
					r.Header.Del(k)
				}
			}
			return nil
		},
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		res.Err = err
		res.FetchClass = model.FetchUnreachable
		res.StatusType = model.StatusUnreachable
		return res, nil
	}
	// Per-host UA when a func is wired (the daemon's per-site trust signal), else
	// the static UserAgent. The hostname is taken from the parsed request URL so the
	// daemon's closure can resolve host -> site -> verification state.
	ua := f.opts.UserAgent
	if f.opts.UserAgentFunc != nil {
		ua = f.opts.UserAgentFunc(httpReq.URL.Hostname())
	}
	httpReq.Header.Set("User-Agent", ua)
	// Intentionally do NOT set Accept-Encoding: the transport adds it itself and
	// then transparently decompresses the gzip response. Setting it manually
	// suppresses that automatic decompression (net/http.Transport contract), so
	// resp.Body would be raw gzip bytes — garbage to goquery/simhash.
	if req.ETag != "" {
		httpReq.Header.Set("If-None-Match", req.ETag)
	}
	if req.LastMod != "" {
		httpReq.Header.Set("If-Modified-Since", req.LastMod)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if req.BasicUser != "" || req.BasicPass != "" {
		httpReq.SetBasicAuth(req.BasicUser, req.BasicPass)
	}
	for _, c := range req.Cookies {
		httpReq.AddCookie(c)
	}

	start := time.Now()
	resp, err := client.Do(httpReq)
	res.ResponseTime = time.Since(start)
	if err != nil {
		res.Err = err
		res.FetchClass = model.FetchUnreachable
		res.StatusType = model.StatusUnreachable
		res.RedirectChain = chain
		return res, nil
	}
	defer func() { _ = resp.Body.Close() }()

	res.HTTPStatus = resp.StatusCode
	res.Header = resp.Header
	res.FinalURL = resp.Request.URL.String()
	res.RedirectChain = chain
	res.NotModified = resp.StatusCode == http.StatusNotModified

	// A redirect loop / over-long chain returns the truncated final 3xx with no
	// transport error. Treat it as unreachable (a broken/looping URL) rather than
	// FetchOK so its 3xx body is never extracted or snapshotted as page content.
	if redirectCapHit {
		res.Err = errTooManyRedirects
		res.FetchClass = model.FetchUnreachable
		res.StatusType = model.StatusUnreachable
		return res, nil
	}

	if res.NotModified {
		res.FetchClass = model.FetchOK
		res.StatusType = model.StatusPage
		return res, nil
	}

	// Read one byte past the cap so an oversized body is detectable: LimitReader
	// returns EOF exactly at its limit, so reading MaxBodyBytes alone cannot tell a
	// body that fits from one truncated at the cap. If we read more than the cap,
	// flag Truncated and trim back to the cap so downstream sees a bounded prefix it
	// knows is incomplete (§5A: a silently-truncated snapshot would hide changes
	// beyond the cutoff).
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.opts.MaxBodyBytes+1))
	if err != nil {
		res.Err = err
		res.FetchClass = model.FetchUnreachable
		res.StatusType = model.StatusUnreachable
		return res, nil
	}
	if int64(len(body)) > f.opts.MaxBodyBytes {
		res.Truncated = true
		body = body[:f.opts.MaxBodyBytes]
	}

	class, detector := Classify(resp.StatusCode, resp.Header, body)
	res.FetchClass = class
	res.Detector = detector
	res.StatusType = StatusTypeFor(resp.StatusCode, len(chain) > 1)

	// Body is only retained for ok-class fetches (§5A: no content snapshot otherwise).
	if class == model.FetchOK {
		res.Body = body
	}
	return res, nil
}

// sameHost reports whether two URLs target the same host (case-insensitive),
// used to decide when to strip per-site credential headers on redirect.
func sameHost(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Hostname(), b.Hostname())
}

// credentialHeaderKeys returns the per-site header names that may carry secrets
// (API keys / bearer tokens in custom headers) and must be dropped on a
// cross-host redirect. The standard sensitive headers are stripped by stdlib.
func credentialHeaderKeys(headers map[string]string) []string {
	if len(headers) == 0 {
		return nil
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "auth") || strings.Contains(lk, "token") ||
			strings.Contains(lk, "key") || strings.Contains(lk, "secret") ||
			strings.HasPrefix(lk, "x-api") {
			keys = append(keys, k)
		}
	}
	return keys
}
