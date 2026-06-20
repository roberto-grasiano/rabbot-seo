package mcpsrv

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// StatusOutput is the get_status tool's structured output. On a reachable daemon it
// carries the StatusResponse fields; on a down daemon Error holds the friendly
// message and the status fields are zero (errors-as-data — a down daemon is reported
// as a clean object, never a tool error, so the model can self-describe and retry).
type StatusOutput struct {
	control.StatusResponse
	Error string `json:"error,omitempty"`
}

// SitesOutput is the list_sites tool's structured output. Sites is always non-nil
// (serializes as [] not null). Error is set (and Sites empty) on a down daemon.
type SitesOutput struct {
	Sites []SiteView `json:"sites"`
	Error string     `json:"error,omitempty"`
}

// getStatusHandler implements the get_status read tool. The empty input struct{}
// carries no arguments. A returned nil *CallToolResult lets the SDK populate Content
// from the typed Out (StatusOutput).
func getStatusHandler(b Bridge) mcp.ToolHandlerFor[struct{}, StatusOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, StatusOutput, error) {
		st, err := b.Status(ctx)
		if err != nil {
			return nil, StatusOutput{Error: err.Error()}, nil
		}
		return nil, StatusOutput{StatusResponse: st}, nil
	}
}

// listSitesHandler implements the list_sites read tool. Sites is normalized to a
// non-nil slice so the JSON is [] not null.
func listSitesHandler(b Bridge) mcp.ToolHandlerFor[struct{}, SitesOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, SitesOutput, error) {
		sites, err := b.Sites(ctx)
		if err != nil {
			return nil, SitesOutput{Sites: []SiteView{}, Error: err.Error()}, nil
		}
		if sites == nil {
			sites = []SiteView{}
		}
		return nil, SitesOutput{Sites: sites}, nil
	}
}

// GetSiteInput is the get_site tool input. SiteID comes from list_sites.
type GetSiteInput struct {
	SiteID int64 `json:"site_id" jsonschema:"the numeric id of the monitored site (from list_sites)"`
}

// SiteDetailOutput wraps the per-site detail. On a down daemon, Error is set and
// Detail is zero. An unknown id is reported via Detail.NotFound (not Error).
type SiteDetailOutput struct {
	Detail SiteDetail `json:"detail"`
	Error  string     `json:"error,omitempty"`
}

func getSiteHandler(b Bridge) mcp.ToolHandlerFor[GetSiteInput, SiteDetailOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetSiteInput) (*mcp.CallToolResult, SiteDetailOutput, error) {
		d, err := b.Site(ctx, in.SiteID)
		if err != nil {
			return nil, SiteDetailOutput{Error: err.Error()}, nil
		}
		return nil, SiteDetailOutput{Detail: d}, nil
	}
}

// ListIssuesInput is the list_issues tool input. All fields are optional: an empty
// severity/status means no filter on that dimension; site_id 0 means all sites.
type ListIssuesInput struct {
	SiteID   int64  `json:"site_id,omitempty" jsonschema:"optional: only issues for this site id (0 = all sites)"`
	Severity string `json:"severity,omitempty" jsonschema:"optional severity filter: critical, warning, or info"`
	Status   string `json:"status,omitempty" jsonschema:"optional status filter: open, closed, or ignored"`
	Segment  string `json:"segment,omitempty" jsonschema:"optional: only issues whose URL is in the named segment (discover names via get_site); an unknown name returns an empty list"`
}

// IssuesOutput wraps the issue list. Issues is always non-nil; Error is set on a
// down daemon.
type IssuesOutput struct {
	Issues []IssueView `json:"issues"`
	Error  string      `json:"error,omitempty"`
}

func listIssuesHandler(b Bridge) mcp.ToolHandlerFor[ListIssuesInput, IssuesOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ListIssuesInput) (*mcp.CallToolResult, IssuesOutput, error) {
		q := IssueQuery{Severity: in.Severity, Status: in.Status, Segment: in.Segment}
		if in.SiteID != 0 {
			id := in.SiteID
			q.SiteID = &id
		}
		issues, err := b.Issues(ctx, q)
		if err != nil {
			return nil, IssuesOutput{Issues: []IssueView{}, Error: err.Error()}, nil
		}
		if issues == nil {
			issues = []IssueView{}
		}
		return nil, IssuesOutput{Issues: issues}, nil
	}
}

// GetHistoryInput is the get_history tool input. URL is the exact monitored URL
// (the stable handle the daemon resolves to a row id, D11). Since is an optional
// RFC3339 lower bound; empty means all recorded history.
type GetHistoryInput struct {
	URL   string `json:"url" jsonschema:"the exact monitored URL to fetch change history for"`
	Since string `json:"since,omitempty" jsonschema:"optional RFC3339 timestamp; only changes detected at or after this time"`
}

// HistoryOutput wraps a URL's change history. Error is set on a down daemon; an
// unknown URL is reported via History.NotFound (not Error).
type HistoryOutput struct {
	History HistoryView `json:"history"`
	Error   string      `json:"error,omitempty"`
}

func getHistoryHandler(b Bridge) mcp.ToolHandlerFor[GetHistoryInput, HistoryOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetHistoryInput) (*mcp.CallToolResult, HistoryOutput, error) {
		var since time.Time
		if in.Since != "" {
			parsed, perr := time.Parse(time.RFC3339, in.Since)
			if perr != nil {
				// Malformed caller input: a tool error the model can correct, distinct
				// from daemon-down/not-found which are data.
				return nil, HistoryOutput{}, fmt.Errorf("invalid since timestamp %q: want RFC3339 (e.g. 2026-06-05T00:00:00Z)", in.Since)
			}
			since = parsed
		}
		h, err := b.History(ctx, in.URL, since)
		if err != nil {
			return nil, HistoryOutput{Error: err.Error()}, nil
		}
		return nil, HistoryOutput{History: h}, nil
	}
}

// SummarizeChangesInput is the summarize_changes tool input. All optional: since is a
// Go duration (default 168h); site_id 0 = all sites; limit 0 = default 10.
type SummarizeChangesInput struct {
	Since   string `json:"since,omitempty" jsonschema:"optional window as a Go duration, e.g. \"24h\" or \"168h\"; default 168h (7 days). Use this to summarise any ad-hoc period."`
	SiteID  int64  `json:"site_id,omitempty" jsonschema:"optional: only this site id (from list_sites); 0 = all sites"`
	Limit   int    `json:"limit,omitempty" jsonschema:"optional max number of top changed URLs; default 10"`
	Segment string `json:"segment,omitempty" jsonschema:"optional: scope the digest to URLs in the named segment (discover names via get_site); an unknown name yields an empty digest"`
}

// ReportSearchShift is one ADDITIVE GSC W2 search_performance_shift annotation on a
// top-changed URL in a summarize_changes digest (signal 3). It is present ONLY when
// that URL's primary query moved measurably across its change date AND enough
// FINALIZED post-change search data exists to claim the move — the common case
// attaches nothing. Shift carries the raw deltas; Annotation is the one-line human
// rendering. It is NEVER a standalone alert: it enriches an existing changed-URL row.
type ReportSearchShift struct {
	URL        string            `json:"url"`
	Shift      store.SearchShift `json:"shift"`
	Annotation string            `json:"annotation"`
}

// ReportOutput wraps the digest. Error is set (and Report zero) on a down daemon —
// errors-as-data, never a tool crash. SearchShifts carries the additive
// search_performance_shift annotations correlated for the digest's top changed URLs
// (empty unless a URL qualifies under the finalized-data discipline) — computed at
// this read layer from the daemon's already-wired search-performance reads.
type ReportOutput struct {
	Report       ReportView          `json:"report"`
	SearchShifts []ReportSearchShift `json:"search_shifts,omitempty"`
	Error        string              `json:"error,omitempty"`
}

const (
	summarizeDefaultWindow = 168 * time.Hour
	summarizeDefaultTopN   = 10
)

func summarizeChangesHandler(b Bridge) mcp.ToolHandlerFor[SummarizeChangesInput, ReportOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in SummarizeChangesInput) (*mcp.CallToolResult, ReportOutput, error) {
		d := summarizeDefaultWindow
		if in.Since != "" {
			parsed, perr := time.ParseDuration(in.Since)
			if perr != nil {
				// Malformed caller input: a tool error the model can correct.
				return nil, ReportOutput{}, fmt.Errorf("invalid since duration %q: want a Go duration like \"24h\" or \"168h\"", in.Since)
			}
			d = parsed
		}
		topN := in.Limit
		if topN <= 0 {
			topN = summarizeDefaultTopN
		}
		q := ReportQuery{Since: time.Now().UTC().Add(-d), TopN: topN, Segment: in.Segment}
		if in.SiteID != 0 {
			id := in.SiteID
			q.SiteID = &id
		}
		v, err := b.Report(ctx, q)
		if err != nil {
			return nil, ReportOutput{Error: mapBridgeError(err)}, nil
		}
		// GSC W2 signal 3: additively annotate the top changed URLs with a
		// search_performance_shift when enough FINALIZED post-change search data
		// exists. This reaches the SAME read path the daemon serves: the metrics come
		// from the already-wired SearchPerformance read; the correlation is the
		// relocated store.SearchPerformanceShift. Absent/partial data attaches nothing.
		shifts := annotateReportSearchShifts(ctx, b, v.TopURLs, time.Now().UTC())
		return nil, ReportOutput{Report: v, SearchShifts: shifts}, nil
	}
}

// annotateReportSearchShifts computes the additive search_performance_shift
// enrichment for each top-changed URL in a digest. For every row it reads that URL's
// stored GSC search metrics (via the daemon's already-wired SearchPerformance read,
// bounded to cover the before/after correlation windows around the change day) and
// runs store.SearchPerformanceShift; only rows with enough FINALIZED post-change data
// on a primary query yield an annotation. A daemon-down/no-data read for one URL is
// skipped silently — the digest never fails because an enrichment could not be
// computed, and nothing is ever fabricated. now is the dataState=final clock.
func annotateReportSearchShifts(ctx context.Context, b Bridge, top []ReportURLChange, now time.Time) []ReportSearchShift {
	var out []ReportSearchShift
	for _, u := range top {
		changeAt, perr := time.Parse(time.RFC3339, u.LastChanged)
		if perr != nil {
			continue // unparseable change timestamp → cannot anchor a correlation
		}
		changeDay := changeAt.UTC().Format("2006-01-02")
		// Bound the read to the before-window's start (changeDay − the shift window,
		// with a day of slack) so SearchPerformanceShift sees the full baseline.
		since := changeAt.UTC().AddDate(0, 0, -(searchShiftReadLookbackDays)).Format(time.RFC3339)
		perf, err := b.SearchPerformance(ctx, u.URL, since)
		if err != nil || !perf.HasData {
			continue // daemon down or no metrics on record → no annotation, never an error
		}
		metrics := searchMetricsFromViews(u.URL, perf.Rows)
		if shift, ok := store.SearchPerformanceShift(metrics, changeDay, now); ok {
			out = append(out, ReportSearchShift{
				URL:        u.URL,
				Shift:      shift,
				Annotation: shift.String(),
			})
		}
	}
	return out
}

// searchShiftReadLookbackDays bounds the per-URL SearchPerformance read for the shift
// correlation: a day of slack beyond the 7-day before-window so the full baseline is
// fetched (the after-window is always newer than the change day, so it is covered by
// reading forward to now). It is a read bound only — the finalized/window gating still
// lives entirely in store.SearchPerformanceShift.
const searchShiftReadLookbackDays = 8

// searchMetricsFromViews maps the wire SearchMetricView rows back to the
// model.SearchMetric shape store.SearchPerformanceShift consumes, stamping the URL
// (the view rows omit it — the read was already scoped to one URL).
func searchMetricsFromViews(url string, rows []SearchMetricView) []model.SearchMetric {
	out := make([]model.SearchMetric, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.SearchMetric{
			URL:         url,
			Query:       r.Query,
			Date:        r.Date,
			Clicks:      r.Clicks,
			Impressions: r.Impressions,
			CTR:         r.CTR,
			Position:    r.Position,
		})
	}
	return out
}

// GetCoverageInput is the get_coverage tool input. SiteID comes from list_sites.
type GetCoverageInput struct {
	SiteID int64 `json:"site_id" jsonschema:"the numeric id of the monitored site (from list_sites)"`
}

// CoverageOutput wraps a site's sitemap-coverage drift. Error is set (and Coverage
// zero) on a down daemon — errors-as-data, never a tool crash. An unknown id is
// reported via Coverage.NotFound (not Error).
type CoverageOutput struct {
	Coverage CoverageView `json:"coverage"`
	Error    string       `json:"error,omitempty"`
}

func getCoverageHandler(b Bridge) mcp.ToolHandlerFor[GetCoverageInput, CoverageOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetCoverageInput) (*mcp.CallToolResult, CoverageOutput, error) {
		c, err := b.Coverage(ctx, in.SiteID)
		if err != nil {
			return nil, CoverageOutput{Error: mapBridgeError(err)}, nil
		}
		return nil, CoverageOutput{Coverage: c}, nil
	}
}

// GetRichResultsInput is the get_rich_results tool input. URL is the exact
// monitored URL (the stable handle the daemon resolves to a row id, like
// get_history).
type GetRichResultsInput struct {
	URL string `json:"url" jsonschema:"the exact monitored URL to validate rich-result eligibility for"`
}

// RichResultsOutput wraps a URL's rich-result eligibility report. Error is set
// (and RichResults zero) on a down daemon — errors-as-data, never a tool crash. An
// unknown URL is reported via RichResults.NotFound (not Error).
type RichResultsOutput struct {
	RichResults RichResultsView `json:"rich_results"`
	Error       string          `json:"error,omitempty"`
}

func getRichResultsHandler(b Bridge) mcp.ToolHandlerFor[GetRichResultsInput, RichResultsOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetRichResultsInput) (*mcp.CallToolResult, RichResultsOutput, error) {
		rr, err := b.RichResults(ctx, in.URL)
		if err != nil {
			return nil, RichResultsOutput{Error: mapBridgeError(err)}, nil
		}
		return nil, RichResultsOutput{RichResults: rr}, nil
	}
}

// GetHealthScoreInput is the get_health_score tool input. SiteID is required; Segment
// optionally scopes to a named segment (discover names via get_site); Since is an
// optional Go duration (default 168h) — the SAME contract as summarize_changes —
// bounding the persisted trend returned in the series.
type GetHealthScoreInput struct {
	SiteID  int64  `json:"site_id" jsonschema:"the numeric id of the monitored site (from list_sites)"`
	Segment string `json:"segment,omitempty" jsonschema:"optional: scope the score to the named segment (discover names via get_site); an unknown name is reported as not_found"`
	Since   string `json:"since,omitempty" jsonschema:"optional trend window as a Go duration, e.g. \"24h\" or \"168h\"; default 168h (7 days) — bounds the returned series, not the live current score"`
}

// HealthScoreOutput wraps a scope's LIVE health score + persisted trend. Error is set
// (and Health zero) on a down daemon — errors-as-data, never a tool crash. An unknown
// site/segment is reported via Health.NotFound (not Error).
type HealthScoreOutput struct {
	Health ScoreView `json:"health"`
	Error  string    `json:"error,omitempty"`
}

// healthScoreDefaultWindow mirrors summarizeDefaultWindow: the trend defaults to the
// last 7 days when no `since` is given.
const healthScoreDefaultWindow = summarizeDefaultWindow

func getHealthScoreHandler(b Bridge) mcp.ToolHandlerFor[GetHealthScoreInput, HealthScoreOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetHealthScoreInput) (*mcp.CallToolResult, HealthScoreOutput, error) {
		d := healthScoreDefaultWindow
		if in.Since != "" {
			parsed, perr := time.ParseDuration(in.Since)
			if perr != nil {
				// Malformed caller input: a tool error the model can correct.
				return nil, HealthScoreOutput{}, fmt.Errorf("invalid since duration %q: want a Go duration like \"24h\" or \"168h\"", in.Since)
			}
			d = parsed
		}
		q := HealthQuery{SiteID: in.SiteID, Segment: in.Segment, Since: time.Now().UTC().Add(-d)}
		v, err := b.HealthScore(ctx, q)
		if err != nil {
			return nil, HealthScoreOutput{Error: mapBridgeError(err)}, nil
		}
		return nil, HealthScoreOutput{Health: v}, nil
	}
}

// blastRadiusDefaultLinkers / whatLinksToDefaultLinkers bound the ranked-linker
// list each tool requests over the shared GET /v1/links endpoint. blast_radius is
// summary-first (a small sample of top linkers is enough context); what_links_to is
// the ranked-list tool (a larger default). Both still report the EXACT inlink_total
// regardless of the cap.
const (
	blastRadiusDefaultLinkers = 5
	whatLinksToDefaultLinkers = 20
)

// BlastRadiusInput is the blast_radius tool input. URL is the exact monitored URL
// (the stable handle, like get_history). Limit optionally caps the sample of top
// linkers returned alongside the summary (the summary counts are always exact).
type BlastRadiusInput struct {
	URL   string `json:"url" jsonschema:"the exact internal URL to measure inbound blast radius for; identity is exact-string (fragment-stripped only), so https://site/a, https://site/a/, and https://site/a?utm=x are DISTINCT nodes"`
	Limit int    `json:"limit,omitempty" jsonschema:"optional cap on the sample of top linking pages returned with the summary; default 5. The inlink/high-importance counts are always exact regardless of this cap."`
}

// LinksOutput wraps a URL's inbound link answers (summary + ranked linkers). Error
// is set (and Links zero) on a down daemon — errors-as-data, never a tool crash. A
// never-linked / unknown URL is reported via Links.NotFound (not Error).
type LinksOutput struct {
	Links LinksView `json:"links"`
	Error string    `json:"error,omitempty"`
}

func blastRadiusHandler(b Bridge) mcp.ToolHandlerFor[BlastRadiusInput, LinksOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in BlastRadiusInput) (*mcp.CallToolResult, LinksOutput, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = blastRadiusDefaultLinkers
		}
		v, err := b.BlastRadius(ctx, in.URL, limit)
		if err != nil {
			return nil, LinksOutput{Error: mapBridgeError(err)}, nil
		}
		return nil, LinksOutput{Links: v}, nil
	}
}

// WhatLinksToInput is the what_links_to tool input. URL is the exact monitored URL;
// Limit caps the ranked linker list (the exact total is always reported).
type WhatLinksToInput struct {
	URL   string `json:"url" jsonschema:"the exact internal URL to list inbound linkers for; identity is exact-string (fragment-stripped only), so https://site/a, https://site/a/, and https://site/a?utm=x are DISTINCT nodes"`
	Limit int    `json:"limit,omitempty" jsonschema:"optional max number of inbound linkers to return, ranked by source importance; default 20. inlink_total is always the exact full count even when this caps the list."`
}

func whatLinksToHandler(b Bridge) mcp.ToolHandlerFor[WhatLinksToInput, LinksOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in WhatLinksToInput) (*mcp.CallToolResult, LinksOutput, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = whatLinksToDefaultLinkers
		}
		v, err := b.WhatLinksTo(ctx, in.URL, limit)
		if err != nil {
			return nil, LinksOutput{Error: mapBridgeError(err)}, nil
		}
		return nil, LinksOutput{Links: v}, nil
	}
}

// GetLinkGraphInput is the get_link_graph tool input. SiteID is required. Focus
// selects the focus-neighborhood mode (the ≤ 2-hop in+out neighborhood of that URL);
// leaving Focus empty yields the segment/folder overview. Hops is the focus radius
// (≤ 2; a value > 2 is rejected as a tool error). Limit overrides the node cap
// downward only. Mode optionally forces "focus" or "overview".
type GetLinkGraphInput struct {
	SiteID int64  `json:"site_id" jsonschema:"the numeric id of the monitored site (from list_sites)"`
	Focus  string `json:"focus,omitempty" jsonschema:"optional focus URL: returns its bounded in+out neighborhood (focus mode). Identity is exact-string (fragment-stripped only). Omit for a whole-site segment/folder overview."`
	Hops   int    `json:"hops,omitempty" jsonschema:"optional focus-mode neighborhood radius, 0-2 (default 2); a value greater than 2 is rejected"`
	Limit  int    `json:"limit,omitempty" jsonschema:"optional cap on the number of nodes returned (downward only; the server enforces a hard ceiling regardless). When the export is clipped, truncated=true and total_nodes/total_edges are bounded by the server ceiling (a floor — at least this many — when clipped)."`
	Mode   string `json:"mode,omitempty" jsonschema:"optional explicit mode: \"focus\" or \"overview\". Defaults to focus when a focus URL is given, otherwise overview."`
}

// LinkGraphOutput wraps the bounded link-graph export. Error is set (and Graph zero)
// on a down daemon — errors-as-data, never a tool crash. An unknown site id is
// reported via Graph.NotFound (not Error). A hops > 2 input is a tool error the model
// can correct.
type LinkGraphOutput struct {
	Graph GraphView `json:"graph"`
	Error string    `json:"error,omitempty"`
}

func getLinkGraphHandler(b Bridge) mcp.ToolHandlerFor[GetLinkGraphInput, LinkGraphOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetLinkGraphInput) (*mcp.CallToolResult, LinkGraphOutput, error) {
		if in.Hops > maxLinkGraphHops {
			// Malformed caller input: a tool error the model can correct, distinct
			// from daemon-down/not-found which are data.
			return nil, LinkGraphOutput{}, fmt.Errorf("hops must be <= %d (got %d)", maxLinkGraphHops, in.Hops)
		}
		v, err := b.GetLinkGraph(ctx, GraphQuery{
			SiteID: in.SiteID,
			Mode:   in.Mode,
			Focus:  in.Focus,
			Hops:   in.Hops,
			Limit:  in.Limit,
		})
		if err != nil {
			return nil, LinkGraphOutput{Error: mapBridgeError(err)}, nil
		}
		return nil, LinkGraphOutput{Graph: v}, nil
	}
}

// maxLinkGraphHops mirrors control.MaxGraphHops / linkgraph.MaxFocusHops: the hard
// hop ceiling. A get_link_graph request for more is a tool error (rejected before
// the bridge round trip), distinct from daemon-down/not-found which are data.
const maxLinkGraphHops = 2

// ── GSC W2 read tools: get_index_status + get_search_performance ──────────────

// GetIndexStatusInput is the get_index_status tool input. URL is the exact
// monitored URL (the stable handle the daemon resolves, like get_history /
// get_rich_results).
type GetIndexStatusInput struct {
	URL string `json:"url" jsonschema:"the exact monitored URL to fetch the latest Google Search Console index status for"`
}

// IndexStatusOutput wraps a URL's latest GSC index status. Error is set (and
// IndexStatus zero) on a down daemon — errors-as-data, never a tool crash. An
// un-inspected URL is reported via IndexStatus.HasStatus=false / NotFound=true
// (the quota-bounded-staleness guard), NOT a discrepancy and NOT an error.
type IndexStatusOutput struct {
	IndexStatus IndexStatusView `json:"index_status"`
	Error       string          `json:"error,omitempty"`
}

func getIndexStatusHandler(b Bridge) mcp.ToolHandlerFor[GetIndexStatusInput, IndexStatusOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetIndexStatusInput) (*mcp.CallToolResult, IndexStatusOutput, error) {
		v, err := b.IndexStatus(ctx, in.URL)
		if err != nil {
			return nil, IndexStatusOutput{Error: mapBridgeError(err)}, nil
		}
		return nil, IndexStatusOutput{IndexStatus: v}, nil
	}
}

// GetSearchPerformanceInput is the get_search_performance tool input. URL is the
// exact monitored URL; Since is an optional Go duration (default 168h) — the SAME
// contract as get_health_score / summarize_changes — bounding the returned rows.
type GetSearchPerformanceInput struct {
	URL   string `json:"url" jsonschema:"the exact monitored URL to fetch Google Search Console search performance (clicks/impressions/CTR/position per query and day) for"`
	Since string `json:"since,omitempty" jsonschema:"optional window as a Go duration, e.g. \"168h\" or \"720h\"; default 168h (7 days). Only finalized (dataState=final) days are stored, so the most recent ~3 days are excluded by design."`
}

// SearchPerformanceOutput wraps a URL's stored GSC search metrics. Error is set
// (and SearchPerformance zero) on a down daemon — errors-as-data, never a tool
// crash. A URL with no metrics is reported via SearchPerformance.HasData=false (the
// quota-bounded honesty), NOT an error.
type SearchPerformanceOutput struct {
	SearchPerformance SearchPerformanceView `json:"search_performance"`
	Error             string                `json:"error,omitempty"`
}

// searchPerformanceDefaultWindow mirrors summarizeDefaultWindow: the search-metrics
// window defaults to the last 7 days when no `since` is given.
const searchPerformanceDefaultWindow = summarizeDefaultWindow

func getSearchPerformanceHandler(b Bridge) mcp.ToolHandlerFor[GetSearchPerformanceInput, SearchPerformanceOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetSearchPerformanceInput) (*mcp.CallToolResult, SearchPerformanceOutput, error) {
		d := searchPerformanceDefaultWindow
		if in.Since != "" {
			parsed, perr := time.ParseDuration(in.Since)
			if perr != nil {
				// Malformed caller input: a tool error the model can correct, distinct
				// from daemon-down/no-data which are data.
				return nil, SearchPerformanceOutput{}, fmt.Errorf("invalid since duration %q: want a Go duration like \"168h\" or \"720h\"", in.Since)
			}
			d = parsed
		}
		// Resolve to an absolute RFC3339 lower bound the daemon understands (the store
		// filters search_metrics by the date portion of `since`).
		since := time.Now().UTC().Add(-d).Format(time.RFC3339)
		v, err := b.SearchPerformance(ctx, in.URL, since)
		if err != nil {
			return nil, SearchPerformanceOutput{Error: mapBridgeError(err)}, nil
		}
		// Normalize Rows to a non-nil slice so the JSON is [] not null even when the
		// URL has no metrics (the production controlBridge already does this, but the
		// handler guards it too so the tool contract holds for any Bridge impl).
		if v.Rows == nil {
			v.Rows = []SearchMetricView{}
		}
		return nil, SearchPerformanceOutput{SearchPerformance: v}, nil
	}
}

// registerReadTools registers the read-only MCP tools against the server. They
// all carry ReadOnlyHint:true — DestructiveHint/OpenWorldHint are meaningful only
// when ReadOnlyHint==false (SDK ToolAnnotations doc), so they are left nil. Reads
// complement, not replace, the three @-mentionable resources (D3): a model invokes a
// tool to read on demand.
func registerReadTools(s *mcp.Server, b Bridge) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_status",
		Title:       "Get daemon status",
		Description: "Returns the Rabbot-SEO daemon status: version, uptime, paused state, and site/URL/queue counts. Reports a friendly error in the payload if the daemon is not running.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getStatusHandler(b))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_sites",
		Title:       "List monitored sites",
		Description: "Lists every monitored site with its id, URL, name, enabled flag, and verification tier (verified/attested/throttled). Use the id with get_site for detail.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, listSitesHandler(b))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_site",
		Title:       "Get site detail",
		Description: "Returns detail for one monitored site by id: verification tier and method, recheck cadence, open-issue count, the latest crawl's SEO fields (title, meta description, canonical, indexability), page-cap coverage (monitored_pages of max_pages, capped flag; max_pages 0 = unlimited), and the site's configured segments (name, match pattern, member count) — use a segment name as the `segment` filter on list_issues or summarize_changes. An unknown id is reported in the payload (not_found), not as an error.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getSiteHandler(b))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_issues",
		Title:       "List SEO issues",
		Description: "Lists detected SEO issues, optionally filtered by site id, severity (critical/warning/info), status (open/closed/ignored), and segment (a named slice of the site, e.g. content or product — discover the names via get_site). Each issue carries its rule, severity, impact points, and detail.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, listIssuesHandler(b))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_history",
		Title:       "Get URL change history",
		Description: "Returns the recorded SEO change history for one monitored URL (old/new value, field, substantive vs cosmetic, detected-at). Optionally bounded by an RFC3339 'since' timestamp. An unknown URL is reported in the payload (not_found), not as an error.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getHistoryHandler(b))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "summarize_changes",
		Title:       "Summarize recent SEO changes",
		Description: "Returns a windowed, cross-site activity digest over the recorded history: change volume with a substantive/cosmetic split, the top changed URLs, and an issue rollup (open now by severity, opened in window, resolved in window), plus a per-site breakdown when no site is specified. Defaults to the last 7 days; pass `since` (a Go duration like \"24h\") to summarise any period, `site_id` to scope to one site, and `segment` to scope to a named slice of the site (discover the names via get_site). Emits structured facts for you to summarise; reports a friendly error in the payload if the daemon is not running.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, summarizeChangesHandler(b))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_coverage",
		Title:       "Get sitemap coverage drift",
		Description: "Reconciles one monitored site's declared sitemap against the crawled inventory: how many declared URLs are uncrawled, how many were never admitted (page-cap exhaustion or rejects), and how many crawled URLs are absent from the sitemap, plus the sitemap seed's HTTP status and bounded sample URLs per bucket. A site with no watched sitemap yet reports has_sitemap=false. An unknown id is reported in the payload (not_found), not as an error.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getCoverageHandler(b))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_rich_results",
		Title:       "Get rich-result eligibility",
		Description: "Validates one monitored URL's latest-crawl structured data (JSON-LD) against the in-binary Rabbot rich-result profile and reports per-type eligibility: the canonical type and its raw @type, whether it is eligible, and the missing required/any-of properties when it is not. The profile mostly mirrors Google's documented rich-result requirements but encodes a few deliberate Rabbot policy choices stricter than Google's literal wording -- notably Article.headline, which Google lists as recommended (not required) yet Rabbot flags as missing, because a deploy that strips the headline leaves no monitorable Article markup; so a missing property is a Rabbot policy verdict, not always a restatement of a Google requirement. Validation is presence-driven — only the types the site already implements and the profile encodes (Product, Article family, BreadcrumbList) get a verdict; other implemented types are reported only as a neutral 'unprofiled' count, never as an issue or a recommendation to add markup. Includes the profile version. A monitored-but-uncrawled URL reports has_snapshot=false; an unknown URL is reported in the payload (not_found), not as an error.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getRichResultsHandler(b))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_health_score",
		Title:       "Get site health score",
		Description: "Returns the LIVE 0-100 SEO health score for one monitored site (or a named segment of it) plus its persisted trend: the current score with a defined flag, the canonical impact/max masses, the crawl-coverage counts (known_urls/processed_urls — so an undefined cold-start score is self-explaining), open-issue counts by severity, an uncapped per-rule breakdown for ranking what hurts the score most, and the recorded time-series (defaults to the last 7 days; pass `since` as a Go duration like \"24h\"). The score is undefined (defined=false) until at least half the scope's known URLs have been crawled — render that as \"—\", never a fake 100 or 0. Scope to a segment with `segment` (discover the names via get_site). An unknown site id or segment name is reported in the payload (not_found), not as an error.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getHealthScoreHandler(b))

	// A9 link-graph LITE — the agent-forward "ship the questions, not the graph"
	// surface. blast_radius and what_links_to answer "how bad is this URL going
	// dark?" / "what links here?"; get_link_graph hands the agent a bounded JSON
	// neighborhood/overview to DRAW (Rabbot never renders). Node identity is
	// exact-string (fragment-stripped only) — a documented LITE limitation surfaced
	// in every description so the model never assumes /a and /a/ are the same node.
	// There is deliberately NO fourth orphans tool: orphan pages surface via the
	// page_orphaned issue stream (list_issues) and `rabbot links --orphans`.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "blast_radius",
		Title:       "Measure a URL's inbound blast radius",
		Description: "Answers \"how bad is it if this URL goes dark (404s, gets noindexed)?\" for one monitored URL: the number of internal pages that link to it (inlinks), how many of those linkers are high-importance (importance >= 0.70), and a weighted-inlink mass, plus a small sample of the top linking pages. Use this to triage a broken page by the size of its internal-link footprint. Node identity is EXACT-STRING (fragment-stripped only) — https://site/a, https://site/a/, and https://site/a?utm=x are DISTINCT nodes, so pass the exact monitored URL. A URL with no inbound links is reported in the payload (not_found), not as an error.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, blastRadiusHandler(b))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "what_links_to",
		Title:       "List a URL's inbound internal linkers",
		Description: "Lists the internal pages that link TO one monitored URL, ranked by source importance, plus the same blast-radius summary as blast_radius (inlinks, high-importance count, weighted mass). inlink_total is always the EXACT full inbound count even when `limit` caps the returned list. Use this to see WHICH pages point at a target (e.g. before changing or removing it). Node identity is EXACT-STRING (fragment-stripped only) — https://site/a, https://site/a/, and https://site/a?utm=x are DISTINCT nodes. A URL with no inbound links is reported in the payload (not_found), not as an error.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, whatLinksToHandler(b))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_link_graph",
		Title:       "Export a bounded internal-link graph to draw",
		Description: "Returns a BOUNDED internal-link graph as JSON for you to render (e.g. a Mermaid or HTML diagram in chat) — Rabbot never draws; the agent draws. Two modes: pass `focus` for the in+out neighborhood of one URL (focus mode, hops 0-2, default 2 — a value > 2 is rejected), or omit `focus` for a whole-site overview aggregated by segment (or by top-level folder when no segments are configured). Node/edge counts are hard-capped server-side (tens of KB, never megabytes); when the export is clipped, truncated=true and total_nodes/total_edges report at least this many (bounded by the server ceiling) so you can say the graph is larger than shown. Node identity is EXACT-STRING (fragment-stripped only) — https://site/a, https://site/a/, and https://site/a?utm=x are DISTINCT nodes. A great demo: get_link_graph with a focus URL, then draw the 404's blast radius. An unknown site id is reported in the payload (not_found), not as an error.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getLinkGraphHandler(b))

	// GSC W2 read tools — Google's ground truth, read-only, daemon-coherent. Both
	// report ABSENT GSC data honestly (has_status=false / has_data=false) rather
	// than guessing: the URL Inspection quota is ~2000/day/property, so a monitored
	// URL may simply have no inspection on record yet — that is data, never a
	// discrepancy and never an error.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_index_status",
		Title:       "Get Google index status for a URL",
		Description: "Returns the latest Google Search Console URL-inspection result for one monitored URL: Google's verdict (PASS/NEUTRAL/FAIL), coverage_state (e.g. \"Submitted and indexed\", \"Crawled - currently not indexed\", \"Discovered - currently not indexed\"), indexing_state, robots_txt_state, page_fetch_state, the canonical Google chose (google_canonical) vs the one you declared (user_canonical), how it was crawled (crawled_as), when Rabbot last inspected it (inspected_at), and Google's last crawl time. This is Google's ground truth — compare it against Rabbot's own indexability verdict (get_site / get_history). A monitored URL with NO inspection on record (the inspection quota is ~2000/day/property, so coverage is bounded) reports has_status=false / not_found=true — that is honest absent data, NEVER a guess and never a discrepancy. An unknown URL is reported in the payload (not_found), not as an error.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getIndexStatusHandler(b))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_search_performance",
		Title:       "Get Google search performance for a URL",
		Description: "Returns the stored Google Search Console search-performance rows for one monitored URL: clicks, impressions, CTR, and average position per (query, day), newest day first, bounded by `since` (a Go duration, default 168h = 7 days). Only FINALIZED data is stored (dataState=final), so the most recent ~3 days are excluded by design — this is the read view behind change-vs-search correlation (\"did this title change cost impressions on its primary query?\"). It is raw read-only DATA, not an alert: Rabbot deliberately does NOT fire standalone traffic/impression/ranking-drop alerts (seasonality and SERP volatility make them noise). A URL with no metrics on record reports has_data=false (honest absent data), not an error.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getSearchPerformanceHandler(b))
}
