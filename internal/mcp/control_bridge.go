package mcpsrv

import (
	"context"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// controlBridge is the production Bridge. Health, status, AND sites now all flow
// through the loopback control client: GET /v1/sites returns tier-enriched site
// summaries (the verification tier is resolved server-side over the live store), so
// the mcp child no longer opens the SQLite database at all (D2). This makes the
// `rabbot mcp` process fully decoupled from the on-disk DB path for reads.
type controlBridge struct {
	client *control.Client // loopback control client; health/status/sites
}

// NewControlBridge builds the production Bridge from a loopback control client.
// The control token lives only inside the client's Authorization header and is
// never logged or emitted.
func NewControlBridge(client *control.Client) Bridge {
	return &controlBridge{client: client}
}

// Health delegates to the loopback control client.
func (b *controlBridge) Health(ctx context.Context) error {
	return b.client.Health(ctx)
}

// Status delegates to the loopback control client.
func (b *controlBridge) Status(ctx context.Context) (control.StatusResponse, error) {
	return b.client.Status(ctx)
}

// Sites fetches the tier-enriched site list over GET /v1/sites and maps each
// control.SiteSummary onto the read-only SiteView wire DTO. The verification tier
// is already resolved by the daemon, so the bridge does no store access.
func (b *controlBridge) Sites(ctx context.Context) ([]SiteView, error) {
	summaries, err := b.client.Sites(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]SiteView, 0, len(summaries))
	for _, s := range summaries {
		views = append(views, SiteView{
			ID:                s.ID,
			URL:               s.URL,
			Name:              s.Name,
			Enabled:           s.Enabled,
			VerificationState: s.VerificationState,
		})
	}
	return views, nil
}

// Site fetches per-site detail over GET /v1/sites/{id}/detail and maps the control
// response onto the read-only SiteDetail wire DTO. An unknown id is surfaced as data
// (SiteDetail{NotFound: true}) — never a Go error — matching the daemon's structured
// not-found (HTTP 200). A non-nil error means the daemon was unreachable.
func (b *controlBridge) Site(ctx context.Context, id int64) (SiteDetail, error) {
	resp, found, err := b.client.SiteDetailFound(ctx, id)
	if err != nil {
		return SiteDetail{}, err
	}
	if !found {
		return SiteDetail{NotFound: true}, nil
	}
	// Map the configured segments onto the read-only wire DTO; normalize to a
	// non-nil slice so the JSON is [] not null even when no segments are configured.
	segments := make([]SegmentView, 0, len(resp.Segments))
	for _, s := range resp.Segments {
		segments = append(segments, SegmentView{
			Name:        s.Name,
			Match:       s.Match,
			MemberCount: s.MemberCount,
		})
	}
	return SiteDetail{
		ID:                 resp.ID,
		URL:                resp.URL,
		Name:               resp.Name,
		Enabled:            resp.Enabled,
		VerificationState:  resp.VerificationState,
		VerificationMethod: resp.VerificationMethod,
		VerifiedAt:         resp.VerifiedAt,
		LastReverifiedAt:   resp.LastReverifiedAt,
		MinInterval:        resp.MinInterval,
		MaxInterval:        resp.MaxInterval,
		OpenIssueCount:     resp.OpenIssueCount,
		HasSnapshot:        resp.HasSnapshot,
		Title:              resp.Title,
		MetaDescription:    resp.MetaDescription,
		Canonical:          resp.Canonical,
		Indexable:          resp.Indexable,
		IndexabilityReason: resp.IndexabilityReason,
		HTTPStatus:         resp.HTTPStatus,
		FetchedAt:          resp.FetchedAt,
		MonitoredPages:     resp.MonitoredPages,
		MaxPages:           resp.MaxPages,
		Capped:             resp.Capped,
		Segments:           segments,
	}, nil
}

// Issues fetches the filtered issue list over GET /v1/issues and maps each control
// IssueView onto the read-only mcp IssueView wire DTO.
func (b *controlBridge) Issues(ctx context.Context, q IssueQuery) ([]IssueView, error) {
	rows, err := b.client.Issues(ctx, q.SiteID, q.Severity, q.Status, q.Segment)
	if err != nil {
		return nil, err
	}
	views := make([]IssueView, 0, len(rows))
	for _, r := range rows {
		views = append(views, IssueView{
			ID:           r.ID,
			URLID:        r.URLID,
			RuleID:       r.RuleID,
			Status:       r.Status,
			Severity:     r.Severity,
			ImpactPoints: r.ImpactPoints,
			Detail:       r.Detail,
			OpenedAt:     r.OpenedAt,
			LastSeenAt:   r.LastSeenAt,
		})
	}
	return views, nil
}

// History fetches a URL's change history over GET /v1/history and maps the control
// response onto the read-only HistoryView wire DTO. An unknown URL is surfaced as
// data (HistoryView{NotFound: true}), never a Go error.
func (b *controlBridge) History(ctx context.Context, pageURL string, since time.Time) (HistoryView, error) {
	resp, err := b.client.History(ctx, pageURL, since)
	if err != nil {
		return HistoryView{}, err
	}
	changes := make([]ChangeView, 0, len(resp.Changes))
	for _, c := range resp.Changes {
		changes = append(changes, ChangeView{
			Field:       c.Field,
			OldValue:    c.OldValue,
			NewValue:    c.NewValue,
			ChangeClass: c.ChangeClass,
			DetectedAt:  c.DetectedAt,
		})
	}
	return HistoryView{
		URL:      resp.URL,
		NotFound: resp.NotFound,
		Changes:  changes,
	}, nil
}

// AddSite registers a new monitored site via the loopback control client
// (POST /v1/sites). The request shape and the response are the existing control
// DTOs — no mcp-local DTO here, because add-site is a pure command/ack.
func (b *controlBridge) AddSite(ctx context.Context, req control.AddSiteRequest) (control.AddSiteResponse, error) {
	return b.client.AddSite(ctx, req)
}

// Recheck forces an immediate recheck of target (POST /v1/crawl). An empty
// target rechecks all enabled sites — the control endpoint's existing semantics.
func (b *controlBridge) Recheck(ctx context.Context, target string) (control.CrawlResponse, error) {
	return b.client.Crawl(ctx, control.CrawlRequest{Target: target})
}

// Pause turns on the global crawl kill-switch (POST /v1/pause).
func (b *controlBridge) Pause(ctx context.Context) error { return b.client.Pause(ctx) }

// Resume turns off the global crawl kill-switch (POST /v1/resume).
func (b *controlBridge) Resume(ctx context.Context) error { return b.client.Resume(ctx) }

// IgnoreIssue marks an issue ignored (POST /v1/issues/{id}/ignore).
func (b *controlBridge) IgnoreIssue(ctx context.Context, id int64) error {
	return b.client.IgnoreIssue(ctx, id)
}

// TestAlert sends a sample alert through a named notifier (POST /v1/notify/test).
func (b *controlBridge) TestAlert(ctx context.Context, notifier string) error {
	return b.client.NotifyTest(ctx, notifier)
}

// SetConfig delegates to the loopback control client (POST /v1/config). The
// daemon re-validates the key against config.AllowConfigKey authoritatively.
func (b *controlBridge) SetConfig(ctx context.Context, key, value string) error {
	return b.client.SetConfig(ctx, key, value)
}

// VerifyBegin asks the daemon to derive the token + placement instructions
// (action:"begin", no DB write) via POST /v1/verify.
func (b *controlBridge) VerifyBegin(ctx context.Context, siteID int64, method string) (VerifyView, error) {
	return b.verify(ctx, siteID, method, "begin")
}

// VerifyCheck asks the daemon to run the proof fetch and persist the record
// (action:"check") via POST /v1/verify.
func (b *controlBridge) VerifyCheck(ctx context.Context, siteID int64, method string) (VerifyView, error) {
	return b.verify(ctx, siteID, method, "check")
}

// verify is the shared round-trip: it calls the daemon endpoint and maps the
// control response onto the mcp-local VerifyView (the wire is JSON-identical).
func (b *controlBridge) verify(ctx context.Context, siteID int64, method, action string) (VerifyView, error) {
	resp, err := b.client.Verify(ctx, control.VerifyRequest{
		SiteID: siteID,
		Method: method,
		Action: action,
	})
	if err != nil {
		return VerifyView{}, err
	}
	return VerifyView{
		SiteID:       resp.SiteID,
		Method:       resp.Method,
		Token:        resp.Token,
		State:        resp.State,
		Reason:       resp.Reason,
		Instructions: resp.Instructions,
		Throttled:    resp.Throttled,
	}, nil
}

// Report fetches the windowed activity digest over GET /v1/report and maps the
// control response onto the read-only ReportView wire DTO.
func (b *controlBridge) Report(ctx context.Context, q ReportQuery) (ReportView, error) {
	var segPtr *string
	if q.Segment != "" {
		segPtr = &q.Segment
	}
	resp, err := b.client.Report(ctx, q.Since, q.SiteID, q.TopN, segPtr)
	if err != nil {
		return ReportView{}, err
	}
	v := ReportView{
		Since:   resp.Since,
		Until:   resp.Until,
		SiteID:  resp.SiteID,
		Changes: ReportChangeSummary(resp.Changes),
		Issues:  ReportIssueSummary(resp.Issues),
	}
	for _, u := range resp.TopURLs {
		v.TopURLs = append(v.TopURLs, ReportURLChange(u))
	}
	for _, s := range resp.Sites {
		v.Sites = append(v.Sites, ReportSiteRollup(s))
	}
	return v, nil
}

// Coverage fetches a site's sitemap-coverage drift over GET /v1/coverage and maps
// the control response onto the read-only CoverageView wire DTO. An unknown site
// id is surfaced as data (CoverageView{NotFound: true}) — never a Go error —
// matching the daemon's 404; a non-nil error means the daemon was unreachable.
func (b *controlBridge) Coverage(ctx context.Context, siteID int64) (CoverageView, error) {
	resp, found, err := b.client.Coverage(ctx, siteID)
	if err != nil {
		return CoverageView{}, err
	}
	if !found {
		return CoverageView{NotFound: true}, nil
	}
	return CoverageView{
		HasSitemap:           resp.HasSitemap,
		SeedStatus:           resp.SeedStatus,
		SitemappedUncrawled:  resp.SitemappedUncrawled,
		SitemappedUnadmitted: resp.SitemappedUnadmitted,
		CrawledNotInSitemap:  resp.CrawledNotInSitemap,
		SampleUncrawled:      resp.SampleUncrawled,
		SampleNotInSitemap:   resp.SampleNotInSitemap,
	}, nil
}

// RichResults fetches a URL's rich-result eligibility over GET /v1/rich-results
// and maps the control response onto the read-only RichResultsView wire DTO. An
// unknown URL is surfaced as data (RichResultsView{NotFound: true}) — the control
// response already carries that flag (HTTP 200) — never a Go error; a non-nil
// error means the daemon was unreachable. Entities is normalized to a non-nil
// slice so the JSON is [] not null.
func (b *controlBridge) RichResults(ctx context.Context, pageURL string) (RichResultsView, error) {
	resp, err := b.client.RichResults(ctx, pageURL)
	if err != nil {
		return RichResultsView{}, err
	}
	entities := make([]RichResultEntityView, 0, len(resp.Entities))
	for _, e := range resp.Entities {
		entities = append(entities, RichResultEntityView{
			Type:         e.Type,
			RawType:      e.RawType,
			Eligible:     e.Eligible,
			Missing:      e.Missing,
			MissingAnyOf: e.MissingAnyOf,
		})
	}
	return RichResultsView{
		URL:         resp.URL,
		NotFound:    resp.NotFound,
		HasSnapshot: resp.HasSnapshot,
		Profile:     resp.Profile,
		Entities:    entities,
		Unprofiled:  resp.Unprofiled,
	}, nil
}

// HealthScore fetches a scope's LIVE health score + persisted trend over GET
// /v1/score and maps the control response onto the read-only ScoreView wire DTO. An
// unknown site id OR segment name is surfaced as data (ScoreView{NotFound: true}) —
// matching the daemon's errors-as-data not-found — never a Go error; a non-nil error
// means the daemon was unreachable. Series is normalized to a non-nil slice so the
// JSON is [] not null.
func (b *controlBridge) HealthScore(ctx context.Context, q HealthQuery) (ScoreView, error) {
	resp, found, err := b.client.Score(ctx, q.SiteID, q.Segment, q.Since)
	if err != nil {
		return ScoreView{}, err
	}
	if !found {
		return ScoreView{NotFound: true}, nil
	}
	series := make([]ScorePointView, 0, len(resp.Series))
	for _, p := range resp.Series {
		series = append(series, ScorePointView(p))
	}
	return ScoreView{
		SiteID:        resp.SiteID,
		Segment:       resp.Segment,
		SegmentID:     resp.SegmentID,
		Defined:       resp.Defined,
		Score:         resp.Score,
		ImpactMass:    resp.ImpactMass,
		MaxMass:       resp.MaxMass,
		KnownURLs:     resp.KnownURLs,
		ProcessedURLs: resp.ProcessedURLs,
		PageCount:     resp.PageCount,
		OpenCritical:  resp.OpenCritical,
		OpenWarning:   resp.OpenWarning,
		OpenInfo:      resp.OpenInfo,
		Breakdown:     resp.Breakdown,
		Series:        series,
	}, nil
}

// links fetches a URL's inbound link-graph answers over GET /v1/links and maps the
// control response onto the read-only LinksView wire DTO. Both BlastRadius and
// WhatLinksTo share this round trip (the control endpoint returns the summary AND
// the ranked linkers in one shot); they differ only in the limit passed for the
// ranked-linker list. A never-linked URL is surfaced as data
// (LinksView{NotFound: true} on the control response) — never a Go error; a non-nil
// error means the daemon was unreachable. Linkers is normalized to a non-nil slice
// so the JSON is [] not null.
func (b *controlBridge) links(ctx context.Context, pageURL string, limit int) (LinksView, error) {
	resp, err := b.client.Links(ctx, pageURL, limit)
	if err != nil {
		return LinksView{}, err
	}
	linkers := make([]LinkerView, 0, len(resp.Linkers))
	for _, l := range resp.Linkers {
		linkers = append(linkers, LinkerView{URLID: l.URLID, URL: l.URL, Importance: l.Importance})
	}
	return LinksView{
		URL:             resp.URL,
		NotFound:        resp.NotFound,
		Inlinks:         resp.Inlinks,
		InlinkTotal:     resp.InlinkTotal,
		HighImportance:  resp.HighImportance,
		WeightedInlinks: resp.WeightedInlinks,
		Linkers:         linkers,
	}, nil
}

// BlastRadius fetches a URL's inbound blast-radius summary (GET /v1/links). It uses
// the shared links round trip; the blast_radius tool is summary-first, so it still
// carries the ranked linkers the same endpoint returns.
func (b *controlBridge) BlastRadius(ctx context.Context, pageURL string, limit int) (LinksView, error) {
	return b.links(ctx, pageURL, limit)
}

// WhatLinksTo fetches a URL's ranked inbound linkers + summary (GET /v1/links).
func (b *controlBridge) WhatLinksTo(ctx context.Context, pageURL string, limit int) (LinksView, error) {
	return b.links(ctx, pageURL, limit)
}

// GetLinkGraph fetches the bounded link-graph export over GET /v1/graph and maps
// the control response onto the read-only GraphView wire DTO. An unknown site id is
// surfaced as data (GraphView{NotFound: true}) — matching the daemon's errors-as-data
// not-found — never a Go error; a non-nil error means the daemon was unreachable (or
// a caller-fault hops > 2, which the tool layer rejects first). The node/edge/group
// slices stay nil when the mode does not populate them (the omitempty JSON shape is
// preserved); the control DTO is JSON-identical so the fields map straight across.
func (b *controlBridge) GetLinkGraph(ctx context.Context, q GraphQuery) (GraphView, error) {
	resp, found, err := b.client.Graph(ctx, control.GraphQuery{
		SiteID: q.SiteID,
		Mode:   q.Mode,
		Focus:  q.Focus,
		Hops:   q.Hops,
		Limit:  q.Limit,
	})
	if err != nil {
		return GraphView{}, err
	}
	if !found {
		return GraphView{NotFound: true}, nil
	}
	out := GraphView{
		Mode:       resp.Mode,
		Focus:      resp.Focus,
		Hops:       resp.Hops,
		Grouping:   resp.Grouping,
		Truncated:  resp.Truncated,
		TotalNodes: resp.TotalNodes,
		TotalEdges: resp.TotalEdges,
	}
	for _, n := range resp.Nodes {
		out.Nodes = append(out.Nodes, GraphNodeView{
			URL:            n.URL,
			Admitted:       n.Admitted,
			Importance:     n.Importance,
			GraphDepth:     n.GraphDepth,
			InSitemap:      n.InSitemap,
			LastFetchClass: n.LastFetchClass,
		})
	}
	for _, e := range resp.Edges {
		out.Edges = append(out.Edges, GraphEdgeView{From: e.From, To: e.To})
	}
	for _, g := range resp.Groups {
		out.Groups = append(out.Groups, GraphGroupView{Name: g.Name})
	}
	for _, ge := range resp.GroupEdges {
		out.GroupEdges = append(out.GroupEdges, GraphGroupEdgeView{From: ge.From, To: ge.To, Weight: ge.Weight})
	}
	return out, nil
}

// IndexStatus fetches one URL's latest GSC index status over GET /v1/index-status
// and maps the control response onto the read-only IndexStatusView wire DTO. An
// un-inspected URL is surfaced as data (NotFound=true / HasStatus=false) — the
// control response already carries that flag (HTTP 200) — never a Go error; a
// non-nil error means the daemon was unreachable. The control DTO is JSON-identical
// so the fields map straight across.
func (b *controlBridge) IndexStatus(ctx context.Context, pageURL string) (IndexStatusView, error) {
	resp, err := b.client.IndexStatus(ctx, pageURL)
	if err != nil {
		return IndexStatusView{}, err
	}
	return IndexStatusView{
		URL:             resp.URL,
		NotFound:        resp.NotFound,
		HasStatus:       resp.HasStatus,
		Verdict:         resp.Verdict,
		CoverageState:   resp.CoverageState,
		IndexingState:   resp.IndexingState,
		RobotsTxtState:  resp.RobotsTxtState,
		PageFetchState:  resp.PageFetchState,
		GoogleCanonical: resp.GoogleCanonical,
		UserCanonical:   resp.UserCanonical,
		CrawledAs:       resp.CrawledAs,
		InspectedAt:     resp.InspectedAt,
		LastCrawlTime:   resp.LastCrawlTime,
	}, nil
}

// SearchPerformance fetches one URL's stored GSC search metrics over GET
// /v1/search-performance and maps the control response onto the read-only
// SearchPerformanceView wire DTO. since is an RFC3339 lower bound forwarded to the
// daemon. A URL with no metrics is surfaced as data (HasData=false) — never a Go
// error; a non-nil error means the daemon was unreachable. Rows is normalized to a
// non-nil slice so the JSON is [] not null.
func (b *controlBridge) SearchPerformance(ctx context.Context, pageURL, since string) (SearchPerformanceView, error) {
	resp, err := b.client.SearchPerformance(ctx, pageURL, since)
	if err != nil {
		return SearchPerformanceView{}, err
	}
	rows := make([]SearchMetricView, 0, len(resp.Rows))
	for _, r := range resp.Rows {
		rows = append(rows, SearchMetricView{
			Query:       r.Query,
			Date:        r.Date,
			Clicks:      r.Clicks,
			Impressions: r.Impressions,
			CTR:         r.CTR,
			Position:    r.Position,
		})
	}
	return SearchPerformanceView{
		URL:     resp.URL,
		HasData: resp.HasData,
		Since:   resp.Since,
		Until:   resp.Until,
		Rows:    rows,
	}, nil
}
