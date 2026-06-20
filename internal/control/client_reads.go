package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Sites fetches the tier-enriched site list (GET /v1/sites).
func (c *Client) Sites(ctx context.Context) ([]SiteSummary, error) {
	var resp SitesResponse
	if err := c.do(ctx, http.MethodGet, "/v1/sites", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Sites, nil
}

// SiteDetail fetches per-site detail (GET /v1/sites/{id}/detail). When the id is
// unknown the daemon returns a structured not-found payload; SiteDetail decodes it
// into an all-zero SiteDetailResponse — callers that need to distinguish "unknown"
// from a zero-valued site should use SiteDetailFound.
func (c *Client) SiteDetail(ctx context.Context, id int64) (SiteDetailResponse, error) {
	resp, _, err := c.SiteDetailFound(ctx, id)
	return resp, err
}

// SiteDetailFound is SiteDetail with an explicit found indicator: found=false when
// the daemon reported the site id is unknown (structured not-found, HTTP 200).
func (c *Client) SiteDetailFound(ctx context.Context, id int64) (SiteDetailResponse, bool, error) {
	// Decode into a superset that captures BOTH the detail fields and the
	// not_found flag, so a single round-trip distinguishes the two HTTP-200 shapes.
	var raw struct {
		SiteDetailResponse
		NotFound bool `json:"not_found"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/sites/"+strconv.FormatInt(id, 10)+"/detail", nil, &raw); err != nil {
		return SiteDetailResponse{}, false, err
	}
	if raw.NotFound {
		return SiteDetailResponse{}, false, nil
	}
	return raw.SiteDetailResponse, true, nil
}

// Issues fetches filtered issues (GET /v1/issues). A nil siteID omits the site
// filter; empty severity/status/segment omit those filters.
func (c *Client) Issues(ctx context.Context, siteID *int64, severity, status, segment string) ([]IssueView, error) {
	q := url.Values{}
	if siteID != nil {
		q.Set("site_id", strconv.FormatInt(*siteID, 10))
	}
	if severity != "" {
		q.Set("severity", severity)
	}
	if status != "" {
		q.Set("status", status)
	}
	if segment != "" {
		q.Set("segment", segment)
	}
	path := "/v1/issues"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var resp IssuesResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Issues, nil
}

// History fetches a URL's change history (GET /v1/history). A zero `since` returns
// all history. An unknown URL comes back as a HistoryResponse with NotFound=true.
func (c *Client) History(ctx context.Context, pageURL string, since time.Time) (HistoryResponse, error) {
	q := url.Values{}
	q.Set("url", pageURL)
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	var resp HistoryResponse
	if err := c.do(ctx, http.MethodGet, "/v1/history?"+q.Encode(), nil, &resp); err != nil {
		return HistoryResponse{}, err
	}
	return resp, nil
}

// RichResults fetches a monitored URL's rich-result eligibility (GET
// /v1/rich-results). An unknown URL comes back as a RichResultsResponse with
// NotFound=true (errors-as-data, mirroring History) — never a transport error.
func (c *Client) RichResults(ctx context.Context, pageURL string) (RichResultsResponse, error) {
	q := url.Values{}
	q.Set("url", pageURL)
	var resp RichResultsResponse
	if err := c.do(ctx, http.MethodGet, "/v1/rich-results?"+q.Encode(), nil, &resp); err != nil {
		return RichResultsResponse{}, err
	}
	return resp, nil
}

// Report fetches the windowed activity digest (GET /v1/report). A zero `since`
// means all-time; a nil siteID means all sites; top<=0 lets the daemon default it;
// a nil segment omits the segment filter (a non-nil one scopes to that segment).
func (c *Client) Report(ctx context.Context, since time.Time, siteID *int64, top int, segment *string) (ReportResponse, error) {
	q := url.Values{}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	if siteID != nil {
		q.Set("site_id", strconv.FormatInt(*siteID, 10))
	}
	if top > 0 {
		q.Set("top", strconv.Itoa(top))
	}
	if segment != nil && *segment != "" {
		q.Set("segment", *segment)
	}
	path := "/v1/report"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var resp ReportResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return ReportResponse{}, err
	}
	return resp, nil
}

// Score fetches a scope's LIVE health score + trend (GET /v1/score). A nil/empty
// segment scopes to the whole site; a zero `since` returns the whole persisted
// series. An unknown site id OR segment name comes back as HTTP 200 with
// not_found=true, which Score surfaces as found=false with a nil error
// (errors-as-data for the MCP bridge) — mirroring SiteDetailFound's two-shape decode.
func (c *Client) Score(ctx context.Context, siteID int64, segment string, since time.Time) (ScoreResponse, bool, error) {
	q := url.Values{}
	q.Set("site_id", strconv.FormatInt(siteID, 10))
	if segment != "" {
		q.Set("segment", segment)
	}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	// Decode into a superset that captures BOTH the score fields and the not_found
	// flag, so a single round-trip distinguishes the two HTTP-200 shapes.
	var raw struct {
		ScoreResponse
		NotFound bool `json:"not_found"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/score?"+q.Encode(), nil, &raw); err != nil {
		return ScoreResponse{}, false, err
	}
	if raw.NotFound {
		return ScoreResponse{}, false, nil
	}
	return raw.ScoreResponse, true, nil
}

// Links fetches a URL's inbound link-graph answers (GET /v1/links?url=&limit=):
// the ranked inlinkers plus the blast-radius summary (A9). A URL with no inbound
// edges comes back as a LinksResponse with NotFound=true (errors-as-data,
// mirroring RichResults) — never a transport error.
func (c *Client) Links(ctx context.Context, pageURL string, limit int) (LinksResponse, error) {
	q := url.Values{}
	q.Set("url", pageURL)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var resp LinksResponse
	if err := c.do(ctx, http.MethodGet, "/v1/links?"+q.Encode(), nil, &resp); err != nil {
		return LinksResponse{}, err
	}
	return resp, nil
}

// Graph fetches the bounded link-graph export (GET /v1/graph). A nil/empty Focus
// with no explicit Mode yields the overview; a Focus yields the focus
// neighborhood. An unknown site id comes back as HTTP 200 with not_found=true,
// which Graph surfaces as found=false with a nil error (errors-as-data for the
// MCP bridge) — mirroring Score's two-shape decode. A caller-fault (hops > 2) is a
// transport error (the daemon returns 400), which c.do collapses into a generic
// error.
func (c *Client) Graph(ctx context.Context, q GraphQuery) (GraphResponse, bool, error) {
	vals := url.Values{}
	vals.Set("site_id", strconv.FormatInt(q.SiteID, 10))
	if q.Mode != "" {
		vals.Set("mode", q.Mode)
	}
	if q.Focus != "" {
		vals.Set("focus", q.Focus)
	}
	if q.Hops > 0 {
		vals.Set("hops", strconv.Itoa(q.Hops))
	}
	if q.Limit > 0 {
		vals.Set("limit", strconv.Itoa(q.Limit))
	}
	// Decode into a superset that captures BOTH the graph fields and the not_found
	// flag, so a single round-trip distinguishes the two HTTP-200 shapes.
	var raw struct {
		GraphResponse
		NotFound bool `json:"not_found"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/graph?"+vals.Encode(), nil, &raw); err != nil {
		return GraphResponse{}, false, err
	}
	if raw.NotFound {
		return GraphResponse{}, false, nil
	}
	return raw.GraphResponse, true, nil
}

// IndexStatus fetches the latest stored GSC index status for one URL (GET
// /v1/index-status?url=, GSC W2). An un-inspected URL comes back as an
// IndexStatusResponse with NotFound=true / HasStatus=false (errors-as-data,
// mirroring RichResults) — never a transport error. That absent-data shape is the
// quota-bounded-staleness guard the index_status_discrepancy signal relies on.
func (c *Client) IndexStatus(ctx context.Context, pageURL string) (IndexStatusResponse, error) {
	q := url.Values{}
	q.Set("url", pageURL)
	var resp IndexStatusResponse
	if err := c.do(ctx, http.MethodGet, "/v1/index-status?"+q.Encode(), nil, &resp); err != nil {
		return IndexStatusResponse{}, err
	}
	return resp, nil
}

// SearchPerformance fetches the stored GSC search metrics for one URL over a
// window (GET /v1/search-performance?url=&since=, GSC W2). since is an RFC3339
// string (empty => all stored history); it is forwarded verbatim (the daemon
// validates it). A URL with no metrics comes back as a SearchPerformanceResponse
// with HasData=false / empty Rows — never a transport error.
func (c *Client) SearchPerformance(ctx context.Context, pageURL, since string) (SearchPerformanceResponse, error) {
	q := url.Values{}
	q.Set("url", pageURL)
	if since != "" {
		q.Set("since", since)
	}
	var resp SearchPerformanceResponse
	if err := c.do(ctx, http.MethodGet, "/v1/search-performance?"+q.Encode(), nil, &resp); err != nil {
		return SearchPerformanceResponse{}, err
	}
	return resp, nil
}

// Coverage fetches a site's sitemap-coverage drift (GET /v1/coverage?site_id=).
// An unknown site id comes back as HTTP 404, which Coverage surfaces as
// found=false with a nil error (errors-as-data for the MCP bridge) — mirroring
// SiteDetailFound's two-shape contract. A transport error maps to
// ErrDaemonNotRunning and a 401 to ErrUnauthorized. It does its own round-trip
// (not c.do) because c.do collapses every non-200 into a generic error, which
// would erase the 404 the bridge needs to distinguish.
func (c *Client) Coverage(ctx context.Context, siteID int64) (CoverageResponse, bool, error) {
	q := url.Values{}
	q.Set("site_id", strconv.FormatInt(siteID, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/coverage?"+q.Encode(), nil)
	if err != nil {
		return CoverageResponse{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return CoverageResponse{}, false, ErrDaemonNotRunning
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var out CoverageResponse
		if derr := json.NewDecoder(resp.Body).Decode(&out); derr != nil {
			return CoverageResponse{}, false, derr
		}
		return out, true, nil
	case http.StatusNotFound:
		return CoverageResponse{}, false, nil
	case http.StatusUnauthorized:
		return CoverageResponse{}, false, ErrUnauthorized
	default:
		var er ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&er)
		return CoverageResponse{}, false, fmt.Errorf("control GET /v1/coverage: status %d: %s", resp.StatusCode, er.Error)
	}
}
