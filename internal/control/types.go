// Package control implements the loopback HTTP control plane (server + client)
// with bearer-token auth, bound to 127.0.0.1 only.
package control

import "errors"

// Sentinel errors (contracts §8.1).
var (
	ErrDaemonNotRunning = errors.New("daemon not running — start with 'rabbot run' or 'rabbot service start'")
	ErrUnauthorized     = errors.New("rabbot: control token invalid or missing")
	// ErrBadRequest marks a control hook failure caused by caller input (e.g. a
	// malformed site URL or interval) rather than a server fault. Handlers map a
	// hook error that wraps it (errors.Is) to HTTP 400 instead of 500. The
	// cli-layer hook wraps client-side validation errors with this sentinel, so
	// control stays decoupled from internal/store.
	ErrBadRequest = errors.New("rabbot: bad request")
)

type ErrorResponse struct {
	Error string `json:"error"`
}

type StatusResponse struct {
	Version     string   `json:"version"`
	Uptime      string   `json:"uptime"`
	Paused      bool     `json:"paused"`
	SiteCount   int      `json:"site_count"`
	URLCount    int      `json:"url_count"`
	DueCount    int      `json:"due_count"`
	QueueDepth  int      `json:"queue_depth"`
	LastCrawlAt string   `json:"last_crawl_at"`
	EgressIP    []string `json:"egress_ip,omitempty"` // outbound IP(s); populated by M1's richer Status hook via fetcher.EgressIP
	// CappedSites is the count of enabled sites currently at their page cap
	// (monitored >= max_pages_per_site, cap>0). 0 when nothing is capped or all
	// sites are uncapped (max_pages_per_site: 0). Surfaced so `status` flags a
	// silent truncation without an N-site detail walk (Spec A D6).
	CappedSites int `json:"capped_sites,omitempty"`
	// MetricsAddr is the read-only Prometheus /metrics listen address when the
	// listener is enabled (config metrics.addr), else empty (the listener is off
	// by default). Surfaced so `rabbot status` — and MCP get_status, which embeds
	// this StatusResponse — reports whether self-observability is on and where,
	// the read surface the Claude-path agent verifies after running
	// `rabbot observability init` (B2). It is informational only; the metrics
	// surface itself is the unauthenticated GET /metrics endpoint, not the
	// token-authed control plane.
	MetricsAddr string `json:"metrics_addr,omitempty"`
}

type AddSiteRequest struct {
	URL         string `json:"url"`
	Name        string `json:"name,omitempty"`
	MinInterval string `json:"min_interval,omitempty"`
	MaxInterval string `json:"max_interval,omitempty"`
	Speed       int    `json:"speed,omitempty"`
}

type AddSiteResponse struct {
	SiteID int64 `json:"site_id"`
}

type CrawlRequest struct {
	Target string `json:"target"`
}

type CrawlResponse struct {
	Queued int `json:"queued"`
}

type NotifyTestRequest struct {
	Notifier string `json:"notifier"`
}

type ConfigSetRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type OKResponse struct {
	OK bool `json:"ok"`
}

// VerifyRequest drives POST /v1/verify. Action is "begin" (derive token +
// instructions, no DB write) or "check" (run the proof fetch and persist the
// record). Method is well_known|dns|meta. The token is DERIVED daemon-side from
// the per-instance key — it is NEVER carried in the request, so no caller value
// can become the match target.
type VerifyRequest struct {
	SiteID int64  `json:"site_id"`
	Method string `json:"method"`
	Action string `json:"action"`
}

// VerifyResponse is the result of a verify begin/check. Token is the DERIVED,
// PUBLIC proof token (its placement is the proof, not its secrecy). Throttled is
// true unless State == "verified".
type VerifyResponse struct {
	SiteID       int64  `json:"site_id"`
	Method       string `json:"method"`
	Token        string `json:"token"`
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Throttled    bool   `json:"throttled"`
}
