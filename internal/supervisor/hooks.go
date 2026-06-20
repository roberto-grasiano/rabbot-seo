package supervisor

import (
	"context"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// HookDeps carries the daemon-owned closures that back each M1 control hook.
// runDaemon builds these from the live scheduler, crawler, store, config path,
// and fetcher.EgressInfo so the control server can drive them.
type HookDeps struct {
	Reload      func() error
	Pause       func(ctx context.Context, paused bool) error
	Crawl       func(ctx context.Context, req control.CrawlRequest) (control.CrawlResponse, error)
	AddSite     func(ctx context.Context, req control.AddSiteRequest) (control.AddSiteResponse, error)
	RemoveSite  func(ctx context.Context, id int64, purge bool) error
	SetConfig   func(ctx context.Context, req control.ConfigSetRequest) error
	Status      func(ctx context.Context) (control.StatusResponse, error)
	IgnoreIssue func(ctx context.Context, id int64) error
	NotifyTest  func(ctx context.Context, notifier string) error
	ListSites   func(ctx context.Context) ([]control.SiteSummary, error)
	SiteDetail  func(ctx context.Context, id int64) (control.SiteDetailResponse, bool, error)
	Issues      func(ctx context.Context, q control.IssueQuery) ([]control.IssueView, error)
	History     func(ctx context.Context, url string, since time.Time) (control.HistoryResponse, error)
	// Report backs GET /v1/report (the windowed activity digest). A non-nil
	// segment scopes the digest to URLs that are members of a segment with that name.
	Report func(ctx context.Context, since time.Time, siteID *int64, top int, segment *string) (control.ReportResponse, error)
	// Coverage backs GET /v1/coverage (sitemap-coverage drift for one site).
	Coverage func(ctx context.Context, siteID int64) (control.CoverageResponse, bool, error)
	// RichResults backs GET /v1/rich-results (rich-result eligibility for one URL).
	RichResults func(ctx context.Context, url string) (control.RichResultsResponse, error)
	// Score backs GET /v1/score (the LIVE health score for one scope + its trend, A6).
	Score func(ctx context.Context, siteID int64, segment string, since time.Time) (control.ScoreResponse, bool, error)
	// Links backs GET /v1/links (the A9 inbound link-graph answers for one URL). Nil
	// (graph disabled) leaves the route 501.
	Links func(ctx context.Context, url string, limit int) (control.LinksResponse, error)
	// Graph backs GET /v1/graph (the A9 bounded link-graph export). Nil (graph
	// disabled) leaves the route 501.
	Graph func(ctx context.Context, q control.GraphQuery) (control.GraphResponse, bool, error)
	// IndexStatus backs GET /v1/index-status (the GSC W2 latest index status for one
	// URL). Read-only; the daemon's store read.
	IndexStatus func(ctx context.Context, url string) (control.IndexStatusResponse, error)
	// SearchPerformance backs GET /v1/search-performance (the GSC W2 stored search
	// metrics for one URL over a window). Read-only; the daemon's store read.
	SearchPerformance func(ctx context.Context, url, since string) (control.SearchPerformanceResponse, error)
	// Verify backs POST /v1/verify (the daemon-owned begin/check proof flow).
	Verify func(ctx context.Context, req control.VerifyRequest) (control.VerifyResponse, error)
}

// BuildControlHooks maps HookDeps into the control.Hooks struct M0's
// control.NewServer consumes. A nil field leaves that hook nil, so its
// M0-registered route returns 501 Not Implemented until the daemon supplies it.
func BuildControlHooks(d HookDeps) control.Hooks {
	return control.Hooks{
		Reload:            d.Reload,
		Pause:             d.Pause,
		Crawl:             d.Crawl,
		AddSite:           d.AddSite,
		RemoveSite:        d.RemoveSite,
		SetConfig:         d.SetConfig,
		Status:            d.Status,
		IgnoreIssue:       d.IgnoreIssue,
		NotifyTest:        d.NotifyTest,
		ListSites:         d.ListSites,
		SiteDetail:        d.SiteDetail,
		Issues:            d.Issues,
		History:           d.History,
		Report:            d.Report,
		Coverage:          d.Coverage,
		RichResults:       d.RichResults,
		Score:             d.Score,
		Links:             d.Links,
		Graph:             d.Graph,
		IndexStatus:       d.IndexStatus,
		SearchPerformance: d.SearchPerformance,
		Verify:            d.Verify,
	}
}
