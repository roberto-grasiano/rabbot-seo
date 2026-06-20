// Package model holds the shared data entities, enums, and operational constants.
// It has no behavior and no dependencies on other internal packages.
package model

import "time"

// ─── Enums / typed string consts ────────────────────────────────────────────

// Severity is the SEO alert-routing tier.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// StatusType is the coarse page classification derived from the fetch result.
type StatusType string

const (
	StatusPage        StatusType = "page"
	StatusRedirect    StatusType = "redirect"
	StatusMissing     StatusType = "missing"      // 4xx
	StatusServerError StatusType = "server_error" // 5xx
	StatusUnreachable StatusType = "unreachable"  // DNS/TLS/timeout/connection error
)

// IssueStatus is the lifecycle state of an Issue, keyed (url_id, rule_id).
type IssueStatus string

const (
	IssueOpen    IssueStatus = "open"
	IssueClosed  IssueStatus = "closed"
	IssueIgnored IssueStatus = "ignored"
)

// ChangeClass distinguishes substantive content changes from cosmetic churn (via SimHash).
type ChangeClass string

const (
	ChangeSubstantive ChangeClass = "substantive"
	ChangeCosmetic    ChangeClass = "cosmetic"
)

// FetchClass is the access/block classification assigned to every fetch (§5A).
type FetchClass string

const (
	FetchOK          FetchClass = "ok"
	FetchSoftBlock   FetchClass = "soft_block"  // 429/503, honor Retry-After, back off
	FetchHardBlock   FetchClass = "hard_block"  // 403 or WAF/challenge heuristic match
	FetchUnreachable FetchClass = "unreachable" // DNS/TLS/timeout/connection error
)

// RenderMode is the persisted classification of how a page delivers its SEO
// content, recorded on each Snapshot (A8). Its five values MIRROR
// precheck.RenderKind's HINT values exactly. model stays dependency-free, so the
// values are redeclared here as literals rather than imported from precheck
// (precheck imports model — the reverse would cycle). The value-set equality
// against precheck.RenderKind is asserted by a drift-guard test in a package that
// may import both (acceptance #9), NOT here. The zero value ("") reads back as
// "unknown" on render surfaces and represents pre-A8 rows (migration DEFAULT ”).
type RenderMode string

const (
	// RenderServerRendered: the core SEO content/head is present in the initial HTML.
	RenderServerRendered RenderMode = "server_rendered"
	// RenderHydrated: a framework hydration payload is present, so content is
	// recoverable WITHOUT JS even if a root div looks thin.
	RenderHydrated RenderMode = "hydrated"
	// RenderHeadOnlyShell: the SEO head is server-rendered but the body is an empty
	// framework root with no hydration payload — head is monitorable, body is not.
	RenderHeadOnlyShell RenderMode = "head_only_shell"
	// RenderClientShell: empty framework root, very low visible words, no hydration
	// payload — content likely needs JavaScript and is not recoverable.
	RenderClientShell RenderMode = "client_shell"
	// RenderUnknown: signals are mixed or insufficient (also the zero/pre-A8 value).
	RenderUnknown RenderMode = "unknown"
)

// IsShell reports whether the render mode is one of the shell states where SEO
// content is not visible without JavaScript (head_only_shell or client_shell).
// It is the single source of truth for that set: the needs_rendering rule opens
// on exactly these modes, and the alert-resolution path treats leaving them as a
// recovery — so the two cannot drift (pinned by a rules-package guard test).
func (m RenderMode) IsShell() bool {
	return m == RenderHeadOnlyShell || m == RenderClientShell
}

// AlertStatus / IncidentStatus lifecycle for the incident-level alerts table.
type AlertStatus string

const (
	AlertOpen   AlertStatus = "open"
	AlertClosed AlertStatus = "closed"
)

// FileSnapshotKind identifies a file-level monitored entity.
type FileSnapshotKind string

const (
	FileKindRobots  FileSnapshotKind = "robots"
	FileKindSitemap FileSnapshotKind = "sitemap"
)

// ─── Operational alert/change-type consts (NOT SEO severities) ───────────────
const (
	ChangeTypeMonitoringBlocked     = "monitoring_blocked"
	ChangeTypeMonitoringUnreachable = "monitoring_unreachable"
)

// ─── Entities (mirror SQLite tables; NO cwv / enrichment fields) ─────────────

// Site is a monitored site (mirrors the sites table).
type Site struct {
	ID             int64     `db:"id"`
	BaseURL        string    `db:"base_url"`
	Name           string    `db:"name"`
	Enabled        bool      `db:"enabled"`
	MinInterval    int64     `db:"min_interval"`
	MaxInterval    int64     `db:"max_interval"`
	MaxConcurrency int       `db:"max_concurrency"`
	SpeedScale     int       `db:"speed_scale"`
	CreatedAt      time.Time `db:"created_at"`
	UpdatedAt      time.Time `db:"updated_at"`
}

// URL is a monitored page within a site (mirrors the urls table).
type URL struct {
	ID             int64      `db:"id"`
	SiteID         int64      `db:"site_id"`
	URL            string     `db:"url"`
	FirstSeen      time.Time  `db:"first_seen"`
	LastChecked    *time.Time `db:"last_checked"`
	NextCheckAt    time.Time  `db:"next_check_at"`
	Interval       int64      `db:"interval"`
	Importance     float64    `db:"importance"`
	Depth          int        `db:"depth"`
	InSitemap      bool       `db:"in_sitemap"`
	StatusType     StatusType `db:"status_type"`
	ETag           string     `db:"etag"`
	LastModified   string     `db:"last_modified"`
	LastFetchClass FetchClass `db:"last_fetch_class"`
}

// Snapshot is a single observation of a URL's SEO-relevant state (mirrors the snapshots table).
type Snapshot struct {
	ID                     int64      `db:"id"`
	URLID                  int64      `db:"url_id"`
	FetchedAt              time.Time  `db:"fetched_at"`
	HTTPStatus             int        `db:"http_status"`
	RedirectChain          string     `db:"redirect_chain"`
	ResponseTimeMS         int64      `db:"response_time_ms"`
	Title                  string     `db:"title"`
	MetaDescription        string     `db:"meta_description"`
	MetaRobots             string     `db:"meta_robots"`
	XRobotsTag             string     `db:"x_robots_tag"`
	Canonical              string     `db:"canonical"`
	CanonicalType          string     `db:"canonical_type"`
	Hreflang               string     `db:"hreflang"`
	Headings               string     `db:"headings"`
	WordCount              int        `db:"word_count"`
	ContentSHA256          string     `db:"content_sha256"`
	ContentSimhash         uint64     `db:"content_simhash"`
	JSONLD                 string     `db:"jsonld"`
	JSONLDInvalidCount     int        `db:"jsonld_invalid_count"`
	SchemaTypes            string     `db:"schema_types"`
	InternalLinkCount      int        `db:"internal_link_count"`
	ExternalLinkCount      int        `db:"external_link_count"`
	IncomingCanonicalCount int        `db:"incoming_canonical_count"`
	IncomingRedirectCount  int        `db:"incoming_redirect_count"`
	ImageCount             int        `db:"image_count"`
	MissingAltCount        int        `db:"missing_alt_count"`
	OG                     string     `db:"og"`
	Twitter                string     `db:"twitter"`
	Indexable              bool       `db:"indexable"`
	IndexabilityReason     string     `db:"indexability_reason"`
	RenderMode             RenderMode `db:"render_mode"`
	ExtractionSource       string     `db:"extraction_source"`
	RawHTML                []byte     `db:"raw_html"`
}

// Change is a detected field-level difference between two snapshots (mirrors the changes table).
type Change struct {
	ID          int64       `db:"id"`
	URLID       int64       `db:"url_id"`
	SnapshotID  int64       `db:"snapshot_id"`
	Field       string      `db:"field"`
	OldValue    string      `db:"old_value"`
	NewValue    string      `db:"new_value"`
	ChangeClass ChangeClass `db:"change_class"`
	DetectedAt  time.Time   `db:"detected_at"`
}

// Issue is a rule-level finding for a URL, keyed (url_id, rule_id) (mirrors the issues table).
type Issue struct {
	ID           int64       `db:"id"`
	URLID        int64       `db:"url_id"`
	RuleID       string      `db:"rule_id"`
	Status       IssueStatus `db:"status"`
	Severity     Severity    `db:"severity"`
	ImpactPoints int         `db:"impact_points"`
	OpenedAt     time.Time   `db:"opened_at"`
	ClosedAt     *time.Time  `db:"closed_at"`
	LastSeenAt   time.Time   `db:"last_seen_at"`
	Detail       string      `db:"detail"`
}

// Alert is an incident-level, deduplicated notification record (mirrors the alerts table).
type Alert struct {
	ID              int64       `db:"id"`
	SiteID          int64       `db:"site_id"`
	Fingerprint     string      `db:"fingerprint"`
	GroupKey        string      `db:"group_key"`
	Severity        Severity    `db:"severity"`
	Status          AlertStatus `db:"status"`
	AffectedCount   int         `db:"affected_count"`
	FirstDetectedAt time.Time   `db:"first_detected_at"`
	LastUpdatedAt   time.Time   `db:"last_updated_at"`
	LastNotifiedAt  *time.Time  `db:"last_notified_at"`
	AutoClosedAt    *time.Time  `db:"auto_closed_at"`
	PayloadSummary  string      `db:"payload_summary"`
}

// Segment is a named, rule-matched grouping of URLs within a site (mirrors the segments table).
type Segment struct {
	ID        int64  `db:"id"`
	SiteID    int64  `db:"site_id"`
	Name      string `db:"name"`
	MatchRule string `db:"match_rule"`
}

// FileSnapshot is an observation of a site-level file (robots.txt / sitemap.xml) (mirrors the file_snapshots table).
type FileSnapshot struct {
	ID            int64            `db:"id"`
	SiteID        int64            `db:"site_id"`
	Kind          FileSnapshotKind `db:"kind"`
	FetchedAt     time.Time        `db:"fetched_at"`
	ContentSHA256 string           `db:"content_sha256"`
	ParsedEntries string           `db:"parsed_entries"`
	HTTPStatus    int              `db:"http_status"`
}

// SearchMetric is one Google Search Console searchAnalytics.query row at the
// (page, query, date) grain (mirrors the search_metrics table). URL is canonical
// (the same keyspace as urls.url) and site-scoped; Date is the GSC 'YYYY-MM-DD'
// day bucket as the API returns it (a calendar day, not an instant). The metrics
// are Google's ground truth for the day; a re-pull/backfill upserts on
// (SiteID, URL, Query, Date). dataState=final is the puller's concern.
type SearchMetric struct {
	ID          int64   `db:"id"`
	SiteID      int64   `db:"site_id"`
	URL         string  `db:"url"`
	Query       string  `db:"query"`
	Date        string  `db:"date"`
	Clicks      int64   `db:"clicks"`
	Impressions int64   `db:"impressions"`
	CTR         float64 `db:"ctr"`
	Position    float64 `db:"position"`
}

// URLIndexStatus is the latest Google Search Console urlInspection.index.inspect
// result for one URL (mirrors the url_index_status table). URL is canonical (the
// same keyspace as urls.url) and site-scoped; the upsert keeps ONE current row per
// (SiteID, URL) — a fresh inspection overwrites the prior one. InspectedAt is when
// Rabbot pulled it (UTC); LastCrawlTime is Google's last-crawl instant (UTC, nil
// when Google reports none). The string verdict fields are stored verbatim as GSC
// returns them; W2's signals interpret them.
type URLIndexStatus struct {
	ID              int64      `db:"id"`
	SiteID          int64      `db:"site_id"`
	URL             string     `db:"url"`
	InspectedAt     time.Time  `db:"inspected_at"`
	Verdict         string     `db:"verdict"`
	CoverageState   string     `db:"coverage_state"`
	IndexingState   string     `db:"indexing_state"`
	RobotsTxtState  string     `db:"robots_txt_state"`
	PageFetchState  string     `db:"page_fetch_state"`
	GoogleCanonical string     `db:"google_canonical"`
	UserCanonical   string     `db:"user_canonical"`
	CrawledAs       string     `db:"crawled_as"`
	LastCrawlTime   *time.Time `db:"last_crawl_time"`
}
