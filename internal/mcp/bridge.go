package mcpsrv

import (
	"context"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// SiteView is the read-only, mcp-local wire shape for a monitored site. It is a
// deliberate DTO (NOT model.Site or a control type) so the MCP payload can evolve
// in Spec 2 without coupling the wire to the internal store/control shapes — that
// decoupling is the seam. It carries only the fields a read-only client needs.
type SiteView struct {
	ID                int64  `json:"id"`
	URL               string `json:"url"`
	Name              string `json:"name"`
	Enabled           bool   `json:"enabled"`
	VerificationState string `json:"verification_state"`
}

// SiteDetail is the read-only wire DTO for a single monitored site, enriched with
// its verification tier, recheck cadence, open-issue count, and the latest snapshot
// SEO fields. It is JSON-identical to control.SiteDetailResponse (Phase 1) so the
// production bridge can decode the control response straight into it. Snapshot
// fields are zero/"" when the site has no homepage snapshot yet (HasSnapshot=false).
type SiteDetail struct {
	ID                 int64  `json:"id"`
	URL                string `json:"url"`
	Name               string `json:"name"`
	Enabled            bool   `json:"enabled"`
	VerificationState  string `json:"verification_state"`
	VerificationMethod string `json:"verification_method,omitempty"`
	VerifiedAt         string `json:"verified_at,omitempty"`
	LastReverifiedAt   string `json:"last_reverified_at,omitempty"`
	MinInterval        int64  `json:"min_interval_seconds"`
	MaxInterval        int64  `json:"max_interval_seconds"`
	OpenIssueCount     int    `json:"open_issue_count"`
	HasSnapshot        bool   `json:"has_snapshot"`
	Title              string `json:"title,omitempty"`
	MetaDescription    string `json:"meta_description,omitempty"`
	Canonical          string `json:"canonical,omitempty"`
	// Indexable has NO omitempty: a genuine indexable=false must serialize so a
	// consumer can tell "not indexable" apart from "field absent" (no snapshot).
	Indexable          bool   `json:"indexable"`
	IndexabilityReason string `json:"indexability_reason,omitempty"`
	HTTPStatus         int    `json:"http_status,omitempty"`
	FetchedAt          string `json:"fetched_at,omitempty"`
	// Cap visibility (Phase 3): JSON-identical to control.SiteDetailResponse so
	// the production bridge decodes straight in. MaxPages 0 = unlimited.
	MonitoredPages int  `json:"monitored_pages"`
	MaxPages       int  `json:"max_pages"`
	Capped         bool `json:"capped"`
	// Segments lists the site's configured segments (name, match pattern, live
	// member count) so an agent can discover the names to pass as the `segment`
	// filter on list_issues / summarize_changes (A7). JSON-identical to
	// control.SiteDetailResponse.Segments; always non-nil (serializes as [] not null).
	Segments []SegmentView `json:"segments"`
	// NotFound is true when no site with the requested id exists; the rest of the
	// struct is then zero. This is errors-as-data, NOT a Go error (spec §Error handling).
	NotFound bool `json:"not_found,omitempty"`
}

// SegmentView is one configured segment in a SiteDetail: its filterable name, the
// match pattern (anchored URL-path regexp), and its live member count.
// JSON-identical to control.SegmentSummary (the seam).
type SegmentView struct {
	Name        string `json:"name"`
	Match       string `json:"match"`
	MemberCount int    `json:"member_count"`
}

// IssueView is the read-only wire DTO for a single open/closed/ignored issue. It is
// JSON-identical to the control IssueView (Phase 1).
type IssueView struct {
	ID           int64  `json:"id"`
	URLID        int64  `json:"url_id"`
	RuleID       string `json:"rule_id"`
	Status       string `json:"status"`
	Severity     string `json:"severity"`
	ImpactPoints int    `json:"impact_points"`
	Detail       string `json:"detail"`
	OpenedAt     string `json:"opened_at"`
	LastSeenAt   string `json:"last_seen_at"`
}

// IssueQuery is the optional filter set for the Issues read. A nil/zero field means
// "no filter on that dimension". It mirrors control.IssueQuery (Phase 1). Segment
// scopes to issues whose URL is a member of a segment with that name (A7).
type IssueQuery struct {
	SiteID   *int64
	Severity string // "" | critical | warning | info
	Status   string // "" | open | closed | ignored
	Segment  string // "" = no segment filter; otherwise a configured segment name
}

// ChangeView is one change row in a URL's history. JSON-identical to control.ChangeView.
type ChangeView struct {
	Field       string `json:"field"`
	OldValue    string `json:"old_value"`
	NewValue    string `json:"new_value"`
	ChangeClass string `json:"change_class"`
	DetectedAt  string `json:"detected_at"`
}

// HistoryView is the read-only wire DTO for a URL's change history. JSON-identical to
// control.HistoryResponse. NotFound is true (and Changes empty) when the URL is not a
// monitored URL — errors-as-data, not a Go error.
type HistoryView struct {
	URL      string       `json:"url"`
	NotFound bool         `json:"not_found,omitempty"`
	Changes  []ChangeView `json:"changes"`
}

// VerifyView is the read-only mcp-local wire shape for a verify begin/check result.
// It is JSON-identical to control.VerifyResponse so the bridge decodes the control
// response straight into it, while keeping the MCP wire decoupled from control
// types (the DTO seam established by SiteView). The Token is the PUBLIC proof token
// (placement is the proof), never a secret.
type VerifyView struct {
	SiteID       int64  `json:"site_id"`
	Method       string `json:"method"`
	Token        string `json:"token"`
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Throttled    bool   `json:"throttled"`
}

// Report* are the read-only, mcp-local wire DTOs for the activity digest. They are
// JSON-identical to the control.Report* DTOs (the seam), kept decoupled so the MCP
// wire can evolve without coupling to control internals.
type ReportChangeSummary struct {
	Total       int `json:"total"`
	Substantive int `json:"substantive"`
	Cosmetic    int `json:"cosmetic"`
}
type ReportIssueSummary struct {
	OpenTotal      int `json:"open_total"`
	OpenCritical   int `json:"open_critical"`
	OpenWarning    int `json:"open_warning"`
	OpenInfo       int `json:"open_info"`
	OpenedInWindow int `json:"opened_in_window"`
	ClosedInWindow int `json:"closed_in_window"`
}
type ReportURLChange struct {
	URLID       int64  `json:"url_id"`
	URL         string `json:"url"`
	Count       int    `json:"count"`
	LastChanged string `json:"last_changed"`
}
type ReportSiteRollup struct {
	SiteID     int64  `json:"site_id"`
	BaseURL    string `json:"base_url"`
	Changes    int    `json:"changes"`
	OpenIssues int    `json:"open_issues"`
}

// ReportView is the windowed activity digest as the MCP tool returns it. JSON-identical
// to control.ReportResponse so the bridge decodes the control response straight in.
type ReportView struct {
	Since   string              `json:"since"`
	Until   string              `json:"until"`
	SiteID  *int64              `json:"site_id,omitempty"`
	Changes ReportChangeSummary `json:"changes"`
	Issues  ReportIssueSummary  `json:"issues"`
	TopURLs []ReportURLChange   `json:"top_urls,omitempty"`
	Sites   []ReportSiteRollup  `json:"sites,omitempty"`
}

// ReportQuery is the resolved input the Bridge.Report seam carries. Since is already
// resolved to an absolute UTC time by the tool layer; SiteID nil = all sites;
// Segment (when non-empty) scopes the digest to URLs in a segment with that name (A7).
type ReportQuery struct {
	Since   time.Time
	SiteID  *int64
	TopN    int
	Segment string
}

// CoverageView is the sitemap-coverage drift as the get_coverage MCP tool returns
// it. It is JSON-identical to control.CoverageResponse (and store.SitemapCoverageResult)
// so the production bridge decodes the control response straight in — the seam.
// HasSitemap is false (and the counts zero) for a site with no watched sitemap.
// NotFound is true when the site id is unknown (errors-as-data, not a Go error).
type CoverageView struct {
	HasSitemap bool `json:"has_sitemap"`
	SeedStatus int  `json:"seed_status"`

	SitemappedUncrawled  int `json:"sitemapped_uncrawled"`
	SitemappedUnadmitted int `json:"sitemapped_unadmitted"`
	CrawledNotInSitemap  int `json:"crawled_not_in_sitemap"`

	SampleUncrawled    []string `json:"sample_uncrawled"`
	SampleNotInSitemap []string `json:"sample_not_in_sitemap"`

	NotFound bool `json:"not_found,omitempty"`
}

// RichResultEntityView is one profiled JSON-LD entity's eligibility verdict as the
// get_rich_results MCP tool returns it. JSON-identical to control.RichResultEntity
// (the seam). Type is the canonical profile family; RawType is the literal @type in
// the markup (so an alias like BlogPosting stays visible). Missing names absent
// Required properties; MissingAnyOf lists any-of groups with no present member.
type RichResultEntityView struct {
	Type         string     `json:"type"`
	RawType      string     `json:"raw_type"`
	Eligible     bool       `json:"eligible"`
	Missing      []string   `json:"missing,omitempty"`
	MissingAnyOf [][]string `json:"missing_any_of,omitempty"`
}

// RichResultsView is a URL's rich-result eligibility as the get_rich_results MCP
// tool returns it. JSON-identical to control.RichResultsResponse so the production
// bridge decodes the control response straight in (the ReportView pattern).
// HasSnapshot is false (Entities empty, Profile still set) when the URL is
// monitored but never crawled. NotFound is true when the URL is not monitored —
// errors-as-data, not a Go error. Unprofiled is a neutral count of
// typed-but-unprofiled entities (never an eligibility verdict).
type RichResultsView struct {
	URL         string                 `json:"url"`
	NotFound    bool                   `json:"not_found,omitempty"`
	HasSnapshot bool                   `json:"has_snapshot"`
	Profile     string                 `json:"profile"`
	Entities    []RichResultEntityView `json:"entities"`
	Unprofiled  int                    `json:"unprofiled"`
}

// ScorePointView is one persisted health-score trend point as the get_health_score
// MCP tool returns it. JSON-identical to control.ScorePoint (the seam). ComputedAt
// is RFC3339 (UTC).
type ScorePointView struct {
	ComputedAt   string  `json:"computed_at"`
	Score        float64 `json:"score"`
	ImpactMass   int     `json:"impact_mass"`
	MaxMass      int     `json:"max_mass"`
	PageCount    int     `json:"page_count"`
	OpenCritical int     `json:"open_critical"`
	OpenWarning  int     `json:"open_warning"`
	OpenInfo     int     `json:"open_info"`
}

// ScoreView is one scope's LIVE health score + persisted trend as the get_health_score
// MCP tool returns it. JSON-identical to control.ScoreResponse so the production
// bridge decodes the control response straight in (the ReportView pattern). Defined
// is false (Score meaningless) when the scope has no importance-weighted page mass or
// sits below the cold-start coverage floor — the model renders "—", never a fake
// 100/0; KnownURLs/ProcessedURLs make an undefined cold-start self-explaining.
// Breakdown is the UNCAPPED per-rule mass JSON (ranking attribution, NOT the score
// math). NotFound is true when the site id or segment name is unknown — errors-as-data,
// not a Go error. Series is normalized to a non-nil slice so the JSON is [] not null.
type ScoreView struct {
	SiteID        int64            `json:"site_id"`
	Segment       string           `json:"segment,omitempty"`
	SegmentID     *int64           `json:"segment_id,omitempty"`
	Defined       bool             `json:"defined"`
	Score         float64          `json:"score"`
	ImpactMass    int              `json:"impact_mass"`
	MaxMass       int              `json:"max_mass"`
	KnownURLs     int              `json:"known_urls"`
	ProcessedURLs int              `json:"processed_urls"`
	PageCount     int              `json:"page_count"`
	OpenCritical  int              `json:"open_critical"`
	OpenWarning   int              `json:"open_warning"`
	OpenInfo      int              `json:"open_info"`
	Breakdown     string           `json:"breakdown"`
	Series        []ScorePointView `json:"series"`
	NotFound      bool             `json:"not_found,omitempty"`
}

// HealthQuery is the resolved input the Bridge.HealthScore seam carries. SiteID is
// required; Segment (when non-empty) scopes to a named segment; Since is already
// resolved to an absolute UTC time by the tool layer (same contract as ReportQuery).
type HealthQuery struct {
	SiteID  int64
	Segment string
	Since   time.Time
}

// LinkerView is one inbound source page for a URL as the what_links_to MCP tool
// returns it: the admitted source's id, URL, and importance, for ranking inlinks.
// JSON-identical to control.LinkerView (the seam).
type LinkerView struct {
	URLID      int64   `json:"url_id"`
	URL        string  `json:"url"`
	Importance float64 `json:"importance"`
}

// LinksView is a URL's inbound link-graph answers as the what_links_to / blast_radius
// MCP tools return them: the ranked inlinkers (capped to the requested limit) plus
// the blast-radius summary (inlink count, high-importance count ≥ 0.70, weighted
// inlink mass). InlinkTotal is the EXACT inbound count even when Linkers is
// truncated. NotFound is true when the URL has ZERO inbound edges (a never-linked /
// unknown node) — errors-as-data, not a Go error. Node identity is exact-string,
// fragment-stripped only: /a, /a/, and /a?utm=x are distinct nodes (A9 LITE
// limitation). JSON-identical to control.LinksResponse so the production bridge
// decodes the control response straight in (the ReportView pattern).
type LinksView struct {
	URL             string       `json:"url"`
	NotFound        bool         `json:"not_found,omitempty"`
	Inlinks         int          `json:"inlinks"`
	InlinkTotal     int          `json:"inlink_total"`
	HighImportance  int          `json:"high_importance"`
	WeightedInlinks float64      `json:"weighted_inlinks"`
	Linkers         []LinkerView `json:"linkers"`
}

// GraphNodeView is one node in a focus-mode link-graph export as the get_link_graph
// MCP tool returns it. URL is the exact-string node identity; the rest is populated
// for ADMITTED nodes, while a linked-but-never-admitted target carries URL +
// Admitted=false. JSON-identical to control.GraphNodeView (the seam).
type GraphNodeView struct {
	URL            string  `json:"url"`
	Admitted       bool    `json:"admitted"`
	Importance     float64 `json:"importance"`
	GraphDepth     *int    `json:"graph_depth,omitempty"`
	InSitemap      bool    `json:"in_sitemap"`
	LastFetchClass string  `json:"last_fetch_class,omitempty"`
}

// GraphEdgeView is one directed internal-link edge in a focus-mode export.
// JSON-identical to control.GraphEdgeView (the seam).
type GraphEdgeView struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// GraphGroupView is one aggregated group (segment or folder) in an overview-mode
// export. JSON-identical to control.GraphGroupView (the seam).
type GraphGroupView struct {
	Name string `json:"name"`
}

// GraphGroupEdgeView is one weighted inter-group edge in an overview-mode export.
// JSON-identical to control.GraphGroupEdgeView (the seam).
type GraphGroupEdgeView struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Weight int    `json:"weight"`
}

// GraphView is the bounded link-graph export as the get_link_graph MCP tool returns
// it. Exactly one of (Nodes/Edges) or (Groups/GroupEdges) is populated, per Mode.
// Truncated is true when a server-side cap clipped the response; TotalNodes/TotalEdges
// carry the EXACT full counts so the agent knows the export is a bounded sample.
// Rabbot emits JSON only — the agent draws the graph (Mermaid/HTML in chat). NotFound
// is true when the site id is unknown — errors-as-data, not a Go error. JSON-identical
// to control.GraphResponse so the production bridge decodes the control response
// straight in (the ReportView pattern).
type GraphView struct {
	Mode  string `json:"mode"`
	Focus string `json:"focus,omitempty"`
	Hops  int    `json:"hops,omitempty"`

	Nodes []GraphNodeView `json:"nodes,omitempty"`
	Edges []GraphEdgeView `json:"edges,omitempty"`

	Grouping   string               `json:"grouping,omitempty"`
	Groups     []GraphGroupView     `json:"groups,omitempty"`
	GroupEdges []GraphGroupEdgeView `json:"group_edges,omitempty"`

	Truncated  bool `json:"truncated"`
	TotalNodes int  `json:"total_nodes"`
	TotalEdges int  `json:"total_edges"`

	NotFound bool `json:"not_found,omitempty"`
}

// GraphQuery is the resolved input the Bridge.GetLinkGraph seam carries. SiteID is
// required; Focus (when non-empty) selects the focus-neighborhood mode; Mode wins
// when explicit ("focus" | "overview"); Hops is the focus radius (≤ 2; the tool
// layer rejects > 2 before the bridge); Limit overrides the node cap downward only.
type GraphQuery struct {
	SiteID int64
	Mode   string
	Focus  string
	Hops   int
	Limit  int
}

// IndexStatusView is one URL's latest GSC index status as the get_index_status
// MCP tool returns it (GSC W2). JSON-identical to control.IndexStatusResponse so
// the production bridge decodes the control response straight in (the ReportView
// pattern). HasStatus is false (and the verdict fields empty) when the URL has
// never been inspected — the quota-bounded-staleness guard: a missing
// url_index_status row is reported as has_status=false / not_found=true, NEVER a
// discrepancy and never an error. NotFound mirrors History/RichResults; HasStatus
// mirrors RichResults.HasSnapshot (both name the same absent-data signal).
type IndexStatusView struct {
	URL             string `json:"url"`
	NotFound        bool   `json:"not_found,omitempty"`
	HasStatus       bool   `json:"has_status"`
	Verdict         string `json:"verdict,omitempty"`
	CoverageState   string `json:"coverage_state,omitempty"`
	IndexingState   string `json:"indexing_state,omitempty"`
	RobotsTxtState  string `json:"robots_txt_state,omitempty"`
	PageFetchState  string `json:"page_fetch_state,omitempty"`
	GoogleCanonical string `json:"google_canonical,omitempty"`
	UserCanonical   string `json:"user_canonical,omitempty"`
	CrawledAs       string `json:"crawled_as,omitempty"`
	InspectedAt     string `json:"inspected_at,omitempty"`
	LastCrawlTime   string `json:"last_crawl_time,omitempty"`
}

// SearchMetricView is one (query, date) search-performance row as get_search_performance
// returns it (GSC W2). JSON-identical to control.SearchMetricView (the seam). It is
// the read view of the search_performance_shift correlation data.
type SearchMetricView struct {
	Query       string  `json:"query"`
	Date        string  `json:"date"`
	Clicks      int64   `json:"clicks"`
	Impressions int64   `json:"impressions"`
	CTR         float64 `json:"ctr"`
	Position    float64 `json:"position"`
}

// SearchPerformanceView is one URL's stored GSC search metrics over a window as the
// get_search_performance MCP tool returns it (GSC W2, dataState=final). JSON-identical
// to control.SearchPerformanceResponse so the production bridge decodes the control
// response straight in. HasData is false (Rows empty) when no metrics are on record
// for the URL/window — the quota-bounded honesty of index-status (never an error).
// Rows is normalized to a non-nil slice so the JSON is [] not null.
type SearchPerformanceView struct {
	URL     string             `json:"url"`
	HasData bool               `json:"has_data"`
	Since   string             `json:"since,omitempty"`
	Until   string             `json:"until,omitempty"`
	Rows    []SearchMetricView `json:"rows"`
}

// Bridge is the small, injectable surface the resource handlers depend on instead
// of a concrete *control.Client, so they can be unit-tested against a mock with no
// live daemon and no store. The production implementation (controlBridge) wraps a
// loopback control client (health/status) and a store opener (sites); see the
// package doc for the sites-from-store split and the Spec-2 TODO.
type Bridge interface {
	// Health reports whether the daemon is reachable on the loopback control API.
	// A nil error means healthy; a non-nil error (e.g. control.ErrDaemonNotRunning)
	// means unreachable — handlers map that to data, never to a crashed resource.
	Health(ctx context.Context) error
	// Status returns the daemon's StatusResponse from the loopback control API.
	Status(ctx context.Context) (control.StatusResponse, error)
	// Sites returns the monitored sites as read-only SiteViews.
	Sites(ctx context.Context) ([]SiteView, error)
	// Site returns the per-site detail DTO for the given site id. An unknown id is
	// reported as data (SiteDetail.NotFound=true), not as a Go error; a Go error
	// means the daemon was unreachable.
	Site(ctx context.Context, id int64) (SiteDetail, error)
	// Issues returns the filtered issue list (open/closed/ignored, optionally scoped
	// to a site and/or severity).
	Issues(ctx context.Context, q IssueQuery) ([]IssueView, error)
	// History returns a monitored URL's change history at or after `since` (zero =
	// all). An unknown URL is reported as data (HistoryView.NotFound=true).
	History(ctx context.Context, url string, since time.Time) (HistoryView, error)

	// ── New writes (Phase 3): each maps 1:1 to an existing control endpoint ──

	// AddSite starts monitoring a new site (POST /v1/sites).
	AddSite(ctx context.Context, req control.AddSiteRequest) (control.AddSiteResponse, error)
	// Recheck forces an immediate recheck of a target (POST /v1/crawl); an empty
	// target rechecks all enabled sites.
	Recheck(ctx context.Context, target string) (control.CrawlResponse, error)
	// Pause turns on the global crawl kill-switch (POST /v1/pause).
	Pause(ctx context.Context) error
	// Resume turns off the global crawl kill-switch (POST /v1/resume).
	Resume(ctx context.Context) error
	// IgnoreIssue marks an issue ignored (POST /v1/issues/{id}/ignore).
	IgnoreIssue(ctx context.Context, id int64) error
	// TestAlert sends a sample alert through a named notifier (POST /v1/notify/test).
	TestAlert(ctx context.Context, notifier string) error
	// SetConfig sets an allow-listed config key on the daemon (POST /v1/config).
	// The allow-list guard runs both in the tool layer (fast rejection) and in the
	// daemon hook (authoritative); this method maps 1:1 to control client SetConfig.
	SetConfig(ctx context.Context, key, value string) error

	// VerifyBegin derives the instance-bound token and returns placement
	// instructions for a site. It performs NO DB write (verify_begin is read-only).
	VerifyBegin(ctx context.Context, siteID int64, method string) (VerifyView, error)
	// VerifyCheck runs the daemon-owned proof fetch and persists the proof record,
	// returning the resulting tier + Reason.
	VerifyCheck(ctx context.Context, siteID int64, method string) (VerifyView, error)

	// Report returns the windowed cross-site activity digest (GET /v1/report).
	Report(ctx context.Context, q ReportQuery) (ReportView, error)

	// Coverage returns a site's sitemap-coverage drift (GET /v1/coverage). An
	// unknown site id is reported as data (CoverageView.NotFound=true), not a Go
	// error; a Go error means the daemon was unreachable.
	Coverage(ctx context.Context, siteID int64) (CoverageView, error)

	// RichResults returns a monitored URL's rich-result eligibility (GET
	// /v1/rich-results). An unknown URL is reported as data
	// (RichResultsView.NotFound=true), not a Go error; a Go error means the daemon
	// was unreachable.
	RichResults(ctx context.Context, url string) (RichResultsView, error)

	// HealthScore returns a scope's LIVE health score + persisted trend (GET
	// /v1/score). An unknown site id or segment name is reported as data
	// (ScoreView.NotFound=true), not a Go error; a Go error means the daemon was
	// unreachable.
	HealthScore(ctx context.Context, q HealthQuery) (ScoreView, error)

	// BlastRadius returns a URL's inbound blast-radius summary (GET /v1/links): how
	// many pages link to it and how many are high-importance — "how bad is this URL
	// going dark?". A URL with no inbound edges is reported as data
	// (LinksView.NotFound=true), not a Go error; a Go error means the daemon was
	// unreachable. (Both blast_radius and what_links_to map to GET /v1/links — the
	// summary and the ranked linkers are one round trip.)
	BlastRadius(ctx context.Context, url string, limit int) (LinksView, error)

	// WhatLinksTo returns a URL's ranked inbound linkers (GET /v1/links): the source
	// pages that link to it, importance-ranked, plus the blast-radius summary. A URL
	// with no inbound edges is reported as data (LinksView.NotFound=true), not a Go
	// error; a Go error means the daemon was unreachable.
	WhatLinksTo(ctx context.Context, url string, limit int) (LinksView, error)

	// GetLinkGraph returns the bounded link-graph export (GET /v1/graph): a focus-URL
	// neighborhood (≤ 2 hops) or a segment/folder overview. An unknown site id is
	// reported as data (GraphView.NotFound=true), not a Go error; a Go error means
	// the daemon was unreachable.
	GetLinkGraph(ctx context.Context, q GraphQuery) (GraphView, error)

	// IndexStatus returns one URL's latest GSC index status (GET /v1/index-status,
	// GSC W2). An un-inspected URL is reported as data (IndexStatusView.NotFound=true
	// / HasStatus=false) — the quota-bounded-staleness guard, never a discrepancy and
	// never a Go error; a Go error means the daemon was unreachable.
	IndexStatus(ctx context.Context, url string) (IndexStatusView, error)

	// SearchPerformance returns one URL's stored GSC search metrics over a window
	// (GET /v1/search-performance, GSC W2). since is an RFC3339 lower bound already
	// resolved by the tool layer. A URL with no metrics is reported as data
	// (SearchPerformanceView.HasData=false), not a Go error; a Go error means the
	// daemon was unreachable.
	SearchPerformance(ctx context.Context, url, since string) (SearchPerformanceView, error)
}

// siteViewFromModel maps a store model.Site onto the read-only SiteView wire DTO,
// preserving ID/URL/Name/Enabled. The verification state is passed in separately
// (verState) because it lives on the authoritative proof record, not on model.Site
// — the caller resolves it from store.GetVerification so the MCP payload always
// reports the LIVE throttle tier rather than a stale/absent field.
func siteViewFromModel(s model.Site, verState string) SiteView {
	return SiteView{
		ID:                s.ID,
		URL:               s.BaseURL,
		Name:              s.Name,
		Enabled:           s.Enabled,
		VerificationState: verState,
	}
}
