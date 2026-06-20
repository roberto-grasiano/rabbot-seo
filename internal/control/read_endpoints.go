package control

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

// ─── Handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request) {
	if s.hooks.ListSites == nil {
		notImplemented(w, "list sites")
		return
	}
	sites, err := s.hooks.ListSites(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, SitesResponse{Sites: sites})
}

func (s *Server) handleSiteDetail(w http.ResponseWriter, r *http.Request) {
	if s.hooks.SiteDetail == nil {
		notImplemented(w, "site detail")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid site id"})
		return
	}
	detail, found, err := s.hooks.SiteDetail(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	if !found {
		// Structured not-found as DATA (HTTP 200), per the spec's error handling:
		// the MCP tool surfaces "site not found", never a transport error.
		writeJSON(w, http.StatusOK, NotFoundResponse{Error: "site not found", NotFound: true})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	if s.hooks.Issues == nil {
		notImplemented(w, "list issues")
		return
	}
	q := r.URL.Query()
	var query IssueQuery
	if raw := q.Get("site_id"); raw != "" {
		sid, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid site_id"})
			return
		}
		query.SiteID = &sid
	}
	query.Severity = q.Get("severity")
	query.Status = q.Get("status")
	query.Segment = q.Get("segment")
	issues, err := s.hooks.Issues(r.Context(), query)
	if err != nil {
		// Caller-fault (bad severity/status enum) -> 400; else 500.
		status := http.StatusInternalServerError
		if errorsIsBadRequest(err) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, IssuesResponse{Issues: issues})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.hooks.History == nil {
		notImplemented(w, "history")
		return
	}
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "missing url query parameter"})
		return
	}
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		t, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid since timestamp (want RFC3339)"})
			return
		}
		since = t
	}
	resp, err := s.hooks.History(r.Context(), rawURL, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── GET /v1/sites — list sites, tier-enriched server-side (D2) ──────────────

// SiteSummary is one row of GET /v1/sites. VerificationState is the LIVE throttle
// tier resolved server-side from the proof record (never-verified/empty -> "throttled").
type SiteSummary struct {
	ID                int64  `json:"id"`
	URL               string `json:"url"`
	Name              string `json:"name"`
	Enabled           bool   `json:"enabled"`
	VerificationState string `json:"verification_state"`
}

type SitesResponse struct {
	Sites []SiteSummary `json:"sites"`
}

// ─── GET /v1/sites/{id}/detail — per-site detail ─────────────────────────────

// SiteDetailResponse is the body of GET /v1/sites/{id}/detail. When the site id
// is unknown, the handler instead returns a NotFoundResponse (HTTP 200) so the
// caller surfaces it as data, not an error.
type SiteDetailResponse struct {
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
	// MonitoredPages is the number of URLs currently in the inventory for this
	// site (store.CountSiteURLs). MaxPages is the resolved per-site page cap
	// (0 = unlimited). Capped is MaxPages>0 && MonitoredPages>=MaxPages — a
	// queryable fact so a cap-hit is visible, not just a daemon log line.
	MonitoredPages int  `json:"monitored_pages"`
	MaxPages       int  `json:"max_pages"`
	Capped         bool `json:"capped"`
	// Segments lists this site's configured segments (name, match pattern, live
	// member count) so an agent can discover the filterable names to pass back as
	// the `segment` filter on list_issues / summarize_changes. Always non-nil
	// (serializes as [] not null) so "no segments configured" is unambiguous.
	Segments []SegmentSummary `json:"segments"`
}

// SegmentSummary is one configured segment in a SiteDetailResponse: its name (the
// filterable key), the match pattern (the anchored URL-path regexp), and the live
// member count. JSON-identical to the mcp-local SegmentView (the seam).
type SegmentSummary struct {
	Name        string `json:"name"`
	Match       string `json:"match"`
	MemberCount int    `json:"member_count"`
}

// NotFoundResponse is returned (HTTP 200) by read endpoints when the requested
// entity does not exist, so the MCP tool surfaces a structured not-found payload
// rather than a transport error.
type NotFoundResponse struct {
	Error    string `json:"error"`
	NotFound bool   `json:"not_found"`
}

// ─── GET /v1/issues — filtered issues ────────────────────────────────────────

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

type IssuesResponse struct {
	Issues []IssueView `json:"issues"`
}

// IssueQuery is the parsed GET /v1/issues filter passed to the daemon hook. A nil
// SiteID means "all sites"; empty Severity/Status/Segment mean "no filter on that
// field". Segment scopes to issues whose URL is a member of a segment with that
// name (names are per-site, so an all-sites query filtered by name matches that
// name in any site).
type IssueQuery struct {
	SiteID   *int64
	Severity string
	Status   string
	Segment  string
}

// ─── GET /v1/history?url=&since= — URL change history (D11) ───────────────────

type ChangeView struct {
	Field       string `json:"field"`
	OldValue    string `json:"old_value"`
	NewValue    string `json:"new_value"`
	ChangeClass string `json:"change_class"`
	DetectedAt  string `json:"detected_at"`
}

type HistoryResponse struct {
	URL      string       `json:"url"`
	NotFound bool         `json:"not_found,omitempty"`
	Changes  []ChangeView `json:"changes"`
}

// ─── GET /v1/report?since=&site_id=&top= — windowed activity digest ───────────

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
	LastChanged string `json:"last_changed"` // RFC3339
}

type ReportSiteRollup struct {
	SiteID     int64  `json:"site_id"`
	BaseURL    string `json:"base_url"`
	Changes    int    `json:"changes"`
	OpenIssues int    `json:"open_issues"`
}

// ReportResponse is the body of GET /v1/report. Since/Until are RFC3339 strings
// stamped by the handler (Until = server "now"); SiteID echoes the scope (nil = all
// sites). The hook fills the data fields; the handler fills the envelope.
type ReportResponse struct {
	Since   string              `json:"since"`
	Until   string              `json:"until"`
	SiteID  *int64              `json:"site_id,omitempty"`
	Changes ReportChangeSummary `json:"changes"`
	Issues  ReportIssueSummary  `json:"issues"`
	TopURLs []ReportURLChange   `json:"top_urls,omitempty"`
	Sites   []ReportSiteRollup  `json:"sites,omitempty"`
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	if s.hooks.Report == nil {
		notImplemented(w, "report")
		return
	}
	q := r.URL.Query()
	var since time.Time
	if raw := q.Get("since"); raw != "" {
		t, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid since timestamp (want RFC3339)"})
			return
		}
		since = t
	}
	var siteID *int64
	if raw := q.Get("site_id"); raw != "" {
		sid, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid site_id"})
			return
		}
		siteID = &sid
	}
	top := 0
	if raw := q.Get("top"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid top"})
			return
		}
		top = n
	}
	var segment *string
	if raw := q.Get("segment"); raw != "" {
		segment = &raw
	}
	resp, err := s.hooks.Report(r.Context(), since, siteID, top, segment)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	// Handler stamps the envelope so the data hook stays clock-free/deterministic.
	// Always stamp Since (a zero/omitted since formats as "0001-01-01T00:00:00Z" =
	// "from the beginning"): the wire field is never an empty string, so a consumer
	// — e.g. the MCP ReportView — always sees a valid RFC3339 timestamp, never an
	// ambiguous "".
	resp.SiteID = siteID
	resp.Since = since.UTC().Format(time.RFC3339)
	resp.Until = time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, resp)
}

// ─── GET /v1/coverage?site_id= — sitemap coverage drift (A2) ─────────────────

// CoverageResponse is the body of GET /v1/coverage. It reconciles a site's
// declared sitemap set against the crawled inventory: sitemapped-but-uncrawled
// (declared, admitted, never fetched), sitemapped-but-unadmitted (declared but
// never entered the inventory — page-cap exhaustion / same-host rejects), and
// crawled-but-absent (monitored + fetched, not in the sitemap). HasSitemap is
// false (and every count zero) for a site with no watched sitemap snapshot yet.
// Sample* lists are bounded server-side. The DTO is JSON-identical to the
// store.SitemapCoverageResult and the mcp-local CoverageView (the seam).
type CoverageResponse struct {
	HasSitemap bool `json:"has_sitemap"`
	SeedStatus int  `json:"seed_status"`

	SitemappedUncrawled  int `json:"sitemapped_uncrawled"`
	SitemappedUnadmitted int `json:"sitemapped_unadmitted"`
	CrawledNotInSitemap  int `json:"crawled_not_in_sitemap"`

	SampleUncrawled    []string `json:"sample_uncrawled"`
	SampleNotInSitemap []string `json:"sample_not_in_sitemap"`
}

// handleCoverage backs GET /v1/coverage?site_id=N. A missing/invalid site_id is a
// caller fault -> 400; an unknown site -> 404 (the spec's same-as-handleSiteDetail
// not-found semantics, but as an HTTP status here so the loopback client can map
// it to found=false). A nil hook returns 501.
func (s *Server) handleCoverage(w http.ResponseWriter, r *http.Request) {
	if s.hooks.Coverage == nil {
		notImplemented(w, "coverage")
		return
	}
	raw := r.URL.Query().Get("site_id")
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "missing site_id query parameter"})
		return
	}
	siteID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid site_id"})
		return
	}
	resp, found, err := s.hooks.Coverage(r.Context(), siteID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "site not found"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── GET /v1/rich-results?url= — rich-result eligibility (A4) ────────────────

// RichResultEntity is one profiled JSON-LD entity's verdict. It is JSON-identical
// to the mcp-local RichResultEntityView (the ReportView seam). Type is the
// canonical profile family (e.g. "Article"); RawType is the literal @type in the
// markup (e.g. "BlogPosting"). Missing names absent Required properties;
// MissingAnyOf lists the any-of groups with no present member. Eligible is true
// iff both are empty.
type RichResultEntity struct {
	Type         string     `json:"type"`
	RawType      string     `json:"raw_type"`
	Eligible     bool       `json:"eligible"`
	Missing      []string   `json:"missing,omitempty"`
	MissingAnyOf [][]string `json:"missing_any_of,omitempty"`
}

// RichResultsResponse is the body of GET /v1/rich-results. It is JSON-identical to
// the mcp-local RichResultsView (the seam). HasSnapshot is false (and Entities
// empty, Profile still set) when the URL is monitored but never crawled. NotFound
// is true when the URL is not monitored at all — errors-as-data (HTTP 200),
// matching the History not-found pattern. Profile is the active profile version.
// Unprofiled is a neutral count of typed-but-unprofiled entities (never a verdict).
type RichResultsResponse struct {
	URL         string             `json:"url"`
	NotFound    bool               `json:"not_found,omitempty"`
	HasSnapshot bool               `json:"has_snapshot"`
	Profile     string             `json:"profile"`
	Entities    []RichResultEntity `json:"entities"`
	Unprofiled  int                `json:"unprofiled"`
}

// handleRichResults backs GET /v1/rich-results?url=N. A missing ?url= is a caller
// fault -> 400. An unknown URL is surfaced as data by the hook (NotFound=true,
// HTTP 200), mirroring handleHistory — deliberately NOT a 404 (the single chosen
// not-found pattern; see the fast-follow not-found-divergence note). A nil hook
// returns 501.
func (s *Server) handleRichResults(w http.ResponseWriter, r *http.Request) {
	if s.hooks.RichResults == nil {
		notImplemented(w, "rich results")
		return
	}
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "missing url query parameter"})
		return
	}
	resp, err := s.hooks.RichResults(r.Context(), rawURL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── GET /v1/score?site_id=&segment=&since= — health score (A6) ──────────────

// ScorePoint is one persisted trend point in a ScoreResponse.Series. ComputedAt is
// RFC3339 (UTC). JSON-identical to the mcp-local ScorePointView (the seam).
type ScorePoint struct {
	ComputedAt   string  `json:"computed_at"`
	Score        float64 `json:"score"`
	ImpactMass   int     `json:"impact_mass"`
	MaxMass      int     `json:"max_mass"`
	PageCount    int     `json:"page_count"`
	OpenCritical int     `json:"open_critical"`
	OpenWarning  int     `json:"open_warning"`
	OpenInfo     int     `json:"open_info"`
}

// ScoreResponse is the body of GET /v1/score: one scope's LIVE health score (whole
// site, or a named segment) plus its persisted trend. Defined is false (Score
// meaningless) when the scope has no importance-weighted page mass or sits below the
// cold-start coverage floor — consumers render "—", never a fake 100/0; the coverage
// counts (KnownURLs/ProcessedURLs) make an undefined cold-start self-explaining. When
// the site id or segment name is unknown the handler instead returns a NotFoundResponse
// (HTTP 200) so the caller surfaces it as data, not an error (matching handleSiteDetail).
// Breakdown is the UNCAPPED per-rule mass JSON for ranking attribution (NOT the score
// math). The DTO is JSON-identical across store→control→mcp (the seam).
type ScoreResponse struct {
	SiteID    int64  `json:"site_id"`
	Segment   string `json:"segment,omitempty"`
	SegmentID *int64 `json:"segment_id,omitempty"`
	// Defined has NO omitempty: a genuine defined=false must serialize so a consumer
	// can tell "undefined" from "field absent".
	Defined       bool         `json:"defined"`
	Score         float64      `json:"score"`
	ImpactMass    int          `json:"impact_mass"`
	MaxMass       int          `json:"max_mass"`
	KnownURLs     int          `json:"known_urls"`
	ProcessedURLs int          `json:"processed_urls"`
	PageCount     int          `json:"page_count"`
	OpenCritical  int          `json:"open_critical"`
	OpenWarning   int          `json:"open_warning"`
	OpenInfo      int          `json:"open_info"`
	Breakdown     string       `json:"breakdown"`
	Series        []ScorePoint `json:"series"`
}

// handleScore backs GET /v1/score?site_id=N&segment=&since=. A missing/invalid
// site_id and a malformed since (RFC3339) are caller faults -> 400. An unknown site
// id OR segment name is surfaced as DATA (NotFoundResponse, HTTP 200), matching
// handleSiteDetail — NOT a 404. A nil hook returns 501 (the hooks.Report pattern).
// The path is /v1/score (NOT /v1/health, which is daemon liveness).
func (s *Server) handleScore(w http.ResponseWriter, r *http.Request) {
	if s.hooks.Score == nil {
		notImplemented(w, "score")
		return
	}
	raw := r.URL.Query().Get("site_id")
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "missing site_id query parameter"})
		return
	}
	siteID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid site_id"})
		return
	}
	var since time.Time
	if rawSince := r.URL.Query().Get("since"); rawSince != "" {
		t, perr := time.Parse(time.RFC3339, rawSince)
		if perr != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid since timestamp (want RFC3339)"})
			return
		}
		since = t
	}
	segment := r.URL.Query().Get("segment")

	resp, found, err := s.hooks.Score(r.Context(), siteID, segment, since)
	if err != nil {
		// A caller-fault (e.g. ErrBadRequest from the hook) -> 400; else 500.
		status := http.StatusInternalServerError
		if errorsIsBadRequest(err) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, NotFoundResponse{Error: "site or segment not found", NotFound: true})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── GET /v1/links?url=&limit= — inbound link-graph answers for a URL (A9) ────

// LinkerView is one inbound source page for a URL: the admitted source's id, URL,
// and importance, for ranking inlinks. JSON-identical to the mcp-local LinkerView
// (the seam).
type LinkerView struct {
	URLID      int64   `json:"url_id"`
	URL        string  `json:"url"`
	Importance float64 `json:"importance"`
}

// LinksResponse is the body of GET /v1/links. It answers "what links to this URL,
// and how bad is it going dark?" — the ranked inlinkers (WhatLinksTo, capped to the
// requested limit) plus the blast-radius summary (inlink count, high-importance
// count ≥ 0.70, weighted-inlink mass). InlinkTotal is the EXACT inbound count even
// when Linkers is truncated by the limit. NotFound is true when the URL has ZERO
// inbound edges (a never-linked / unknown node) — errors-as-data (HTTP 200), the
// History not-found pattern, NOT a 404. Node identity is exact-string,
// fragment-stripped only: /a, /a/, and /a?utm=x are distinct nodes (A9 LITE
// limitation). The DTO is JSON-identical to the mcp-local LinksView (the seam).
type LinksResponse struct {
	URL             string       `json:"url"`
	NotFound        bool         `json:"not_found,omitempty"`
	Inlinks         int          `json:"inlinks"`
	InlinkTotal     int          `json:"inlink_total"`
	HighImportance  int          `json:"high_importance"`
	WeightedInlinks float64      `json:"weighted_inlinks"`
	Linkers         []LinkerView `json:"linkers"`
}

// handleLinks backs GET /v1/links?url=&limit=. A missing ?url= is a caller fault ->
// 400; a non-numeric/negative limit is a caller fault -> 400. A URL with no inbound
// edges is surfaced as data (LinksResponse.NotFound=true, HTTP 200), mirroring
// handleRichResults / handleHistory — deliberately NOT a 404. A nil hook returns 501.
func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
	if s.hooks.Links == nil {
		notImplemented(w, "links")
		return
	}
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "missing url query parameter"})
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid limit"})
			return
		}
		limit = n
	}
	resp, err := s.hooks.Links(r.Context(), rawURL, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── GET /v1/graph?site_id=&focus=&hops=&mode=&limit= — bounded export (A9) ───

// GraphNodeView is one node in a focus-mode graph export. URL is the exact-string
// node identity; the remaining fields are populated for ADMITTED nodes, while a
// linked-but-never-admitted target carries URL + Admitted=false. JSON-identical to
// linkgraph.ExportNode (the seam) so the hook decodes the export straight in.
type GraphNodeView struct {
	URL            string  `json:"url"`
	Admitted       bool    `json:"admitted"`
	Importance     float64 `json:"importance"`
	GraphDepth     *int    `json:"graph_depth,omitempty"`
	InSitemap      bool    `json:"in_sitemap"`
	LastFetchClass string  `json:"last_fetch_class,omitempty"`
}

// GraphEdgeView is one directed internal-link edge in a focus-mode graph export.
// JSON-identical to linkgraph.ExportEdge (the seam).
type GraphEdgeView struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// GraphGroupView is one aggregated group (segment or folder) in an overview-mode
// export. JSON-identical to linkgraph.GroupNode (the seam).
type GraphGroupView struct {
	Name string `json:"name"`
}

// GraphGroupEdgeView is one weighted inter-group edge in an overview-mode export.
// JSON-identical to linkgraph.GroupEdge (the seam).
type GraphGroupEdgeView struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Weight int    `json:"weight"`
}

// GraphResponse is the body of GET /v1/graph: the bounded link-graph export. It is
// JSON-identical to linkgraph.Export (the seam) so the hook decodes the export
// straight in and the mcp-local GraphView decodes the control response straight in.
// Exactly one of (Nodes/Edges) or (Groups/GroupEdges) is populated, per Mode. The
// node/edge counts are bounded server-side (config default + a HARD ceiling
// regardless of config); Truncated is true when a cap clipped the response, and
// TotalNodes/TotalEdges are bounded by that ceiling — a floor ("at least this
// many") when Truncated — so the agent knows the export is a bounded sample (the
// agent draws — Rabbot emits JSON only). NotFound is true
// when the site id is unknown — errors-as-data (HTTP 200), the SiteDetail pattern.
type GraphResponse struct {
	Mode  string `json:"mode"`
	Focus string `json:"focus,omitempty"`
	Hops  int    `json:"hops,omitempty"`

	// focus mode
	Nodes []GraphNodeView `json:"nodes,omitempty"`
	Edges []GraphEdgeView `json:"edges,omitempty"`

	// overview mode
	Grouping   string               `json:"grouping,omitempty"`
	Groups     []GraphGroupView     `json:"groups,omitempty"`
	GroupEdges []GraphGroupEdgeView `json:"group_edges,omitempty"`

	Truncated  bool `json:"truncated"`
	TotalNodes int  `json:"total_nodes"`
	TotalEdges int  `json:"total_edges"`

	NotFound bool `json:"not_found,omitempty"`
}

// GraphQuery is the parsed GET /v1/graph request passed to the daemon hook. SiteID
// is required. Mode is "" (the hook defaults to focus when Focus is set, else
// overview), "focus", or "overview". Focus is the focus-mode anchor URL. Hops is
// the focus-mode radius (the handler rejects > 2 with a 400 before reaching the
// hook). Limit overrides the node cap downward only.
type GraphQuery struct {
	SiteID int64
	Mode   string
	Focus  string
	Hops   int
	Limit  int
}

// MaxGraphHops is the hard hop ceiling the handler enforces (mirrors
// linkgraph.MaxFocusHops). A request for more is a caller fault -> 400, rejected
// before the hook runs (criterion 8: "hops=3 rejected clearly").
const MaxGraphHops = 2

// handleGraph backs GET /v1/graph?site_id=&focus=&hops=&mode=&limit=. A
// missing/invalid site_id, a non-numeric/negative limit, a non-numeric hops, and a
// hops > 2 are all caller faults -> 400 (criterion 8). An unknown site id is
// surfaced as data (NotFoundResponse, HTTP 200), matching handleScore. A nil hook
// returns 501.
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	if s.hooks.Graph == nil {
		notImplemented(w, "graph")
		return
	}
	q := r.URL.Query()
	raw := q.Get("site_id")
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "missing site_id query parameter"})
		return
	}
	siteID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid site_id"})
		return
	}
	query := GraphQuery{SiteID: siteID, Mode: q.Get("mode"), Focus: q.Get("focus")}
	if rawHops := q.Get("hops"); rawHops != "" {
		n, perr := strconv.Atoi(rawHops)
		if perr != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid hops"})
			return
		}
		if n > MaxGraphHops {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "hops must be <= 2"})
			return
		}
		query.Hops = n
	}
	if rawLimit := q.Get("limit"); rawLimit != "" {
		n, perr := strconv.Atoi(rawLimit)
		if perr != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid limit"})
			return
		}
		query.Limit = n
	}
	resp, found, err := s.hooks.Graph(r.Context(), query)
	if err != nil {
		// A caller-fault (e.g. an ErrBadRequest-wrapped bad mode or focus-without-url
		// from the hook/export) -> 400; else 500.
		status := http.StatusInternalServerError
		if errorsIsBadRequest(err) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, NotFoundResponse{Error: "site not found", NotFound: true})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// errorsIsBadRequest reports whether err is (or wraps) the caller-fault sentinel
// ErrBadRequest, so the issues handler can map invalid filter enums to HTTP 400.
func errorsIsBadRequest(err error) bool {
	return errors.Is(err, ErrBadRequest)
}
