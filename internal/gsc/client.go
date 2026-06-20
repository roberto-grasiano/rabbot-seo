package gsc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponseBytes caps a googleapis JSON response read so a misbehaving or
// hijacked endpoint cannot exhaust memory. GSC responses are small (a page of
// rows / one inspection); 16 MiB is generous headroom.
const maxResponseBytes = 16 << 20

// Options configures a Client.
type Options struct {
	// Token supplies the bearer for every request (SA or OAuth provider).
	Token TokenProvider
	// HTTPClient is the outbound client. It is the gsc package's OWN client (NOT
	// the SSRF-guarded crawl fetcher): googleapis is public, and the crawl
	// fetcher's FetchClass/redirect/body-cap semantics are wrong for a JSON REST
	// API. A nil client defaults to one with a 30s timeout and bounded transport
	// timeouts (a timeout on every outbound client is a project non-negotiable).
	HTTPClient *http.Client
	// BaseURL overrides the webmasters/v3 host (sites.list, searchAnalytics).
	// Empty → https://www.googleapis.com. Tests point this at an httptest server.
	BaseURL string
	// InspectBaseURL overrides the urlInspection host. Empty →
	// https://searchconsole.googleapis.com. Tests point this at httptest.
	InspectBaseURL string
}

// Client is a typed Google Search Console API client over net/http. It targets
// the three endpoints Rabbot needs and adds the bearer from the injected
// TokenProvider on every call. It never logs the bearer.
type Client struct {
	token          TokenProvider
	http           *http.Client
	baseURL        string
	inspectBaseURL string
}

// NewClient builds a Client. Token is required.
func NewClient(opts Options) (*Client, error) {
	if opts.Token == nil {
		return nil, newSentinel("gsc: NewClient requires a TokenProvider")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = defaultHTTPClient()
	}
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = defaultWebmasters
	}
	inspect := strings.TrimRight(opts.InspectBaseURL, "/")
	if inspect == "" {
		inspect = defaultInspect
	}
	return &Client{
		token:          opts.Token,
		http:           hc,
		baseURL:        base,
		inspectBaseURL: inspect,
	}, nil
}

// defaultHTTPClient builds the package's own client with a timeout on the client
// AND on the transport handshakes/header read (mirrors the fetcher's discipline).
// No SSRF dialControl: googleapis is public, so the crawl guard is unnecessary.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:          20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
		},
	}
}

// SearchAnalyticsQuery runs POST searchAnalytics/query for the property. The
// property is the GSC site identifier — a URL-prefix ("https://ex.com/") or a
// Domain ("sc-domain:ex.com"); it is validated and percent-encoded as one path
// segment.
func (c *Client) SearchAnalyticsQuery(ctx context.Context, property string, req SearchAnalyticsRequest) (*SearchAnalyticsResponse, error) {
	if err := validateProperty(property); err != nil {
		return nil, err
	}
	endpoint := c.baseURL + "/webmasters/v3/sites/" + encodeProperty(property) + "/searchAnalytics/query"
	var out SearchAnalyticsResponse
	if err := c.doJSON(ctx, http.MethodPost, endpoint, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// InspectURL runs POST urlInspection/index:inspect for one URL under a property.
func (c *Client) InspectURL(ctx context.Context, property, inspectionURL string) (*InspectResponse, error) {
	if err := validateProperty(property); err != nil {
		return nil, err
	}
	if inspectionURL == "" {
		return nil, newSentinel("gsc: InspectURL requires an inspection URL")
	}
	endpoint := c.inspectBaseURL + "/v1/urlInspection/index:inspect"
	body := InspectRequest{
		InspectionURL: inspectionURL,
		SiteURL:       property,
		LanguageCode:  "en-US",
	}
	var out InspectResponse
	if err := c.doJSON(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListSites runs GET sites — the verified properties the credential can see. It
// is the lightweight connectivity/auth probe used by doctor.
func (c *Client) ListSites(ctx context.Context) (*SitesListResponse, error) {
	endpoint := c.baseURL + "/webmasters/v3/sites"
	var out SitesListResponse
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// doJSON performs a JSON request/response round-trip with the bearer attached,
// decoding a typed *APIError on a non-2xx. The bearer is never echoed into any
// error. A nil body sends no request body (GET).
func (c *Client) doJSON(ctx context.Context, method, endpoint string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("gsc: marshal request: %w", err)
		}
		reqBody = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return fmt.Errorf("gsc: build request: %w", err)
	}

	// Attach the bearer LAST so a token-provider failure short-circuits before any
	// network call. The token is never placed in an error string.
	tok, err := c.token.Token(ctx)
	if err != nil {
		return fmt.Errorf("gsc: obtain access token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Scrub the bearer defensively: a *url.Error embeds the request URL (no
		// token), but never risk it.
		return fmt.Errorf("gsc: %s %s: %w", method, redactURL(endpoint), scrubTransportErr(err, tok))
	}
	defer func() { _ = resp.Body.Close() }()

	limited := io.LimitReader(resp.Body, maxResponseBytes)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeAPIError(resp.StatusCode, limited)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(limited).Decode(out); err != nil {
		return fmt.Errorf("gsc: decode response: %w", err)
	}
	return nil
}

// decodeAPIError reads a googleapis error envelope into a typed *APIError. If the
// body is not the expected JSON, it still returns an *APIError with the status.
func decodeAPIError(status int, body io.Reader) error {
	raw, _ := io.ReadAll(body)
	var env apiErrorEnvelope
	_ = json.Unmarshal(raw, &env)
	code := env.Error.Code
	if code == 0 {
		code = status
	}
	msg := env.Error.Message
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &APIError{
		HTTPStatus: status,
		Code:       code,
		Status:     env.Error.Status,
		Message:    msg,
	}
}

// validateProperty checks that property is a plausible GSC identifier: a
// "sc-domain:host" Domain property or an "http(s)://…/" URL-prefix property.
func validateProperty(property string) error {
	if property == "" {
		return newSentinel("gsc: property is required")
	}
	if strings.HasPrefix(property, "sc-domain:") {
		if strings.TrimPrefix(property, "sc-domain:") == "" {
			return fmt.Errorf("gsc: invalid domain property %q", property)
		}
		return nil
	}
	u, err := url.Parse(property)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("gsc: property %q must be a URL-prefix (https://ex.com/) or sc-domain:ex.com", property)
	}
	return nil
}

// encodeProperty percent-encodes the property as a single opaque path segment.
// url.PathEscape leaves ":" literal (it is legal mid-path), but the GSC API
// treats {siteUrl} as a single fully-escaped path parameter — matching the
// official generated client, which encodes the colon too. We therefore escape
// ":" as well so "sc-domain:ex.com" → "sc-domain%3Aex.com" and
// "https://ex.com/" → "https%3A%2F%2Fex.com%2F", exactly the form Google expects.
func encodeProperty(property string) string {
	return strings.ReplaceAll(url.PathEscape(property), ":", "%3A")
}

// redactURL returns the endpoint URL with any query string stripped (defensive;
// GSC endpoints carry no secret query params, but keep errors clean).
func redactURL(endpoint string) string {
	if i := strings.IndexByte(endpoint, '?'); i >= 0 {
		return endpoint[:i]
	}
	return endpoint
}

// scrubTransportErr removes the bearer token from a transport error string if it
// somehow appears (it should not — net/http does not put headers in url.Error).
func scrubTransportErr(err error, bearer string) error {
	if bearer == "" {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, bearer) {
		return newSentinel(strings.ReplaceAll(msg, bearer, "<redacted>"))
	}
	return err
}
