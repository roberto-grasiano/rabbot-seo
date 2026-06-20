// Package gsc is a small, hand-rolled Google Search Console API client for the
// three endpoints Rabbot needs — sites.list, searchAnalytics.query, and
// urlInspection.index.inspect — plus the two BYO auth flows (service-account
// JWT and OAuth2 installed-app refresh token).
//
// It deliberately avoids the heavy google.golang.org/api generated client and
// oauth2/google (which pull in gRPC, the OpenTelemetry stack, and the GCE
// metadata module) so the single static CGO_ENABLED=0 binary stays lean. Only
// stdlib (net/http, crypto/rsa, crypto/x509, encoding/{json,pem,base64}) plus
// the already-present golang.org/x/oauth2 CORE package are used.
//
// Secret discipline (CLAUDE.md security surface): the service-account private
// key, the OAuth client secret, and all access/refresh tokens are NEVER logged,
// echoed into errors, or exposed by default struct formatting. The credential
// types implement fmt.Stringer/GoStringer to redact themselves.
package gsc

import (
	"errors"
	"time"
)

// scopeWebmastersReadonly is the OAuth scope for read-only Search Console
// access. Both auth flows request exactly this scope.
const scopeWebmastersReadonly = "https://www.googleapis.com/auth/webmasters.readonly"

// Default Google endpoints. They are constants (no oauth2/google dep needed).
const (
	defaultTokenURI   = "https://oauth2.googleapis.com/token" // #nosec G101 -- public Google OAuth token endpoint URL, not a credential
	defaultAuthURL    = "https://accounts.google.com/o/oauth2/auth"
	defaultWebmasters = "https://www.googleapis.com"
	defaultInspect    = "https://searchconsole.googleapis.com"
)

// SearchAnalyticsRequest is the body of POST searchAnalytics/query. Optional
// fields are omitempty so an unset request does not leak empty knobs (Google
// rejects some empty values, and a clean body is easier to assert in tests).
type SearchAnalyticsRequest struct {
	StartDate             string                 `json:"startDate"`
	EndDate               string                 `json:"endDate"`
	Dimensions            []string               `json:"dimensions,omitempty"`
	Type                  string                 `json:"type,omitempty"`
	DimensionFilterGroups []DimensionFilterGroup `json:"dimensionFilterGroups,omitempty"`
	AggregationType       string                 `json:"aggregationType,omitempty"`
	RowLimit              int                    `json:"rowLimit,omitempty"`
	StartRow              int                    `json:"startRow,omitempty"`
	// DataState gates the partial-data lag: "final" excludes the latest ~3
	// (incomplete) days; "all" includes fresh-but-partial data. Rabbot pulls
	// "final" for alerting per the spec.
	DataState string `json:"dataState,omitempty"`
}

// DimensionFilterGroup is a group of dimension filters (e.g. page == X).
type DimensionFilterGroup struct {
	GroupType string            `json:"groupType,omitempty"`
	Filters   []DimensionFilter `json:"filters,omitempty"`
}

// DimensionFilter is a single dimension/operator/expression filter.
type DimensionFilter struct {
	Dimension  string `json:"dimension"`
	Operator   string `json:"operator,omitempty"`
	Expression string `json:"expression"`
}

// SearchAnalyticsResponse is the searchAnalytics/query result.
type SearchAnalyticsResponse struct {
	Rows                    []SearchAnalyticsRow `json:"rows"`
	ResponseAggregationType string               `json:"responseAggregationType"`
}

// SearchAnalyticsRow is one grouped result. Keys are positional, matching the
// order of the requested Dimensions — the consumer maps them by index against the
// dimensions it requested (the puller requests [date, page, query]).
type SearchAnalyticsRow struct {
	Keys        []string `json:"keys"`
	Clicks      float64  `json:"clicks"`
	Impressions float64  `json:"impressions"`
	CTR         float64  `json:"ctr"`
	Position    float64  `json:"position"`
}

// InspectRequest is the body of urlInspection/index:inspect.
type InspectRequest struct {
	InspectionURL string `json:"inspectionUrl"`
	SiteURL       string `json:"siteUrl"`
	LanguageCode  string `json:"languageCode,omitempty"`
}

// InspectResponse wraps the index inspection result.
type InspectResponse struct {
	InspectionResult InspectionResult `json:"inspectionResult"`
}

// InspectionResult holds the per-aspect inspection results. Rabbot only consumes
// the index-status result in W1.
type InspectionResult struct {
	IndexStatusResult IndexStatusResult `json:"indexStatusResult"`
}

// IndexStatusResult mirrors GSC's IndexStatusInspectionResult — Google's ground
// truth for a single URL's index state and canonicalization. lastCrawlTime is an
// RFC3339 timestamp (or absent for a never-crawled URL → a zero time).
type IndexStatusResult struct {
	Verdict         string    `json:"verdict"`
	CoverageState   string    `json:"coverageState"`
	IndexingState   string    `json:"indexingState"`
	RobotsTxtState  string    `json:"robotsTxtState"`
	PageFetchState  string    `json:"pageFetchState"`
	LastCrawlTime   time.Time `json:"lastCrawlTime"`
	GoogleCanonical string    `json:"googleCanonical"`
	UserCanonical   string    `json:"userCanonical"`
	CrawledAs       string    `json:"crawledAs"`
	Sitemap         []string  `json:"sitemap,omitempty"`
	ReferringURLs   []string  `json:"referringUrls,omitempty"`
}

// SitesListResponse is the sites.list result.
type SitesListResponse struct {
	SiteEntry []SiteEntry `json:"siteEntry"`
}

// SiteEntry is one verified Search Console property and the caller's permission.
type SiteEntry struct {
	SiteURL         string `json:"siteUrl"`
	PermissionLevel string `json:"permissionLevel"`
}

// newSentinel returns a package sentinel error.
func newSentinel(msg string) error { return errors.New(msg) }
