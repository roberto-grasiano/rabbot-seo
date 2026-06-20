package control

import (
	"net/http"
	"time"
)

// GSC W2 read endpoints: GET /v1/index-status and GET /v1/search-performance.
// They are the daemon-coherent read path for the two new GSC MCP tools
// (get_index_status / get_search_performance) and the `rabbot gsc` CLI verbs'
// MCP-equivalent — the daemon's store reads, so the mcp child opens no DB (D2).
//
// Both follow the handleRichResults / handleHistory not-found-as-DATA pattern
// (HTTP 200 with a NotFound/HasStatus/HasData flag), NOT a 404: an un-inspected
// URL or a URL with no search metrics is QUOTA-BOUNDED ABSENCE, never an error and
// never a discrepancy. That honesty is the whole point — Rabbot must report "no
// GSC data on record" rather than guess.

// ─── GET /v1/index-status?url= — latest GSC index status for one URL ──────────

// IndexStatusResponse is the body of GET /v1/index-status: the latest stored
// urlInspection.index.inspect verdict for one URL (mirrors store.URLIndexStatus).
// It is JSON-identical to the mcp-local IndexStatusView (the seam). HasStatus is
// false (and the verdict fields empty) when the URL has never been inspected —
// errors-as-data (HTTP 200), the RichResults not-found pattern; that is the
// quota-bounded-staleness guard (NEVER a discrepancy on missing data). InspectedAt
// is RFC3339 (UTC, when Rabbot pulled it); LastCrawlTime is RFC3339 (UTC) or empty
// when Google reported no last crawl.
type IndexStatusResponse struct {
	URL string `json:"url"`
	// NotFound and HasStatus are the same absent-data signal under two names so a
	// consumer can read either (NotFound mirrors History/RichResults; HasStatus
	// mirrors RichResults.HasSnapshot). HasStatus has NO omitempty so a genuine
	// has_status=false serializes (distinguishable from "field absent").
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

// handleIndexStatus backs GET /v1/index-status?url=. A missing ?url= is a caller
// fault -> 400. An un-inspected URL is surfaced as DATA by the hook (NotFound=true
// / HasStatus=false, HTTP 200), mirroring handleRichResults — deliberately NOT a
// 404. A nil hook returns 501.
func (s *Server) handleIndexStatus(w http.ResponseWriter, r *http.Request) {
	if s.hooks.IndexStatus == nil {
		notImplemented(w, "index status")
		return
	}
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "missing url query parameter"})
		return
	}
	resp, err := s.hooks.IndexStatus(r.Context(), rawURL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── GET /v1/search-performance?url=&since= — GSC search metrics for one URL ───

// SearchMetricView is one (query, date) search-performance row. JSON-identical to
// the mcp-local SearchMetricView (the seam). It is the read view of the
// search_performance_shift correlation data (page → query metrics over a window).
type SearchMetricView struct {
	Query       string  `json:"query"`
	Date        string  `json:"date"` // GSC 'YYYY-MM-DD' calendar day
	Clicks      int64   `json:"clicks"`
	Impressions int64   `json:"impressions"`
	CTR         float64 `json:"ctr"`
	Position    float64 `json:"position"`
}

// SearchPerformanceResponse is the body of GET /v1/search-performance: the stored
// searchAnalytics.query rows for one URL within the requested window (dataState=final
// — the puller only persists finalized days). It is JSON-identical to the mcp-local
// SearchPerformanceView (the seam). HasData is false (and Rows empty) when no metrics
// are on record for the URL/window — errors-as-data (HTTP 200), the quota-bounded
// honesty of index-status. Since/Until echo the resolved window (RFC3339, UTC).
type SearchPerformanceResponse struct {
	URL string `json:"url"`
	// HasData has NO omitempty so a genuine has_data=false serializes (an honest "no
	// search data on record", distinguishable from "field absent").
	HasData bool               `json:"has_data"`
	Since   string             `json:"since,omitempty"`
	Until   string             `json:"until,omitempty"`
	Rows    []SearchMetricView `json:"rows"`
}

// handleSearchPerformance backs GET /v1/search-performance?url=&since=. A missing
// ?url= and a malformed since (RFC3339) are caller faults -> 400 (the handleScore
// since-parse contract). A URL with no metrics is surfaced as data by the hook
// (HasData=false, empty Rows, HTTP 200) — never a 404. The since string is passed
// to the hook verbatim (already validated as RFC3339 here); the handler stamps the
// Until envelope. A nil hook returns 501.
func (s *Server) handleSearchPerformance(w http.ResponseWriter, r *http.Request) {
	if s.hooks.SearchPerformance == nil {
		notImplemented(w, "search performance")
		return
	}
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "missing url query parameter"})
		return
	}
	rawSince := r.URL.Query().Get("since")
	if rawSince != "" {
		if _, perr := time.Parse(time.RFC3339, rawSince); perr != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid since timestamp (want RFC3339)"})
			return
		}
	}
	resp, err := s.hooks.SearchPerformance(r.Context(), rawURL, rawSince)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	// Stamp the Until envelope so the data hook stays clock-free/deterministic; echo
	// the resolved since (empty since => "from the beginning of stored history").
	resp.Since = rawSince
	resp.Until = time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, resp)
}
