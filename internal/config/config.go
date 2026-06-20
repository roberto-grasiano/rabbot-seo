// Package config defines the Config schema and loads/merges/validates it from
// defaults -> config.yaml -> RABBOT_ env -> CLI flags via koanf.
package config

import (
	"errors"
	"net"
	"strings"
	"time"
)

// Sentinel errors (contracts §8.1). ErrRenderJSUnsupported is declared here per
// the canonical contracts; the render-mode field itself is reserved and unused
// in the MVP build (JS rendering deferred — see spec R1), so nothing emits it
// yet. It is kept so M1+ fetch wiring can reference the canonical sentinel.
var (
	ErrContactEmailRequired = errors.New("rabbot: crawler.contact_email is mandatory and must be a valid email address")
	ErrRenderJSUnsupported  = errors.New("rabbot: render: js is not supported in this build (JS rendering deferred)")
)

type Config struct {
	DataDir   string           `koanf:"data_dir"  yaml:"data_dir"`
	Control   ControlConfig    `koanf:"control"   yaml:"control"`
	Log       LogConfig        `koanf:"log"       yaml:"log"`
	Crawler   CrawlerConfig    `koanf:"crawler"   yaml:"crawler"`
	Defaults  DefaultsConfig   `koanf:"defaults"  yaml:"defaults"`
	Sites     []SiteConfig     `koanf:"sites"     yaml:"sites"`
	Notifiers []NotifierConfig `koanf:"notifiers" yaml:"notifiers"`
	Routes    []RouteConfig    `koanf:"routes"    yaml:"routes"`
	Alerting  AlertingConfig   `koanf:"alerting"  yaml:"alerting"`
	Setup     SetupConfig      `koanf:"setup"     yaml:"setup,omitempty"`
	Retention RetentionConfig  `koanf:"retention" yaml:"retention,omitempty"`
	Metrics   MetricsConfig    `koanf:"metrics"   yaml:"metrics,omitempty"`
	Graph     GraphConfig      `koanf:"graph"     yaml:"graph,omitempty"`
}

// GraphConfig configures A9 link-graph LITE: the incremental edge maintenance on
// the crawl path, the periodic click-depth BFS sweep, and the bounded
// get_link_graph export.
//
// Enabled (graph.enabled, default true) is the master switch: when false, run.go
// leaves Crawler.Graph nil (no edge sync, no graph signals) and registers no sweep
// — the scope-gate severability, a no-wiring decision rather than a revert. It is
// one of only TWO graph keys settable over the control plane (allowlist.go) so an
// agent can toggle the feature without a restart.
//
// SweepInterval (graph.sweep_interval, default 6h) is the click-depth BFS cadence
// — the gocron job beside the retention sweep. It is the OTHER control-plane-
// settable graph key.
//
// MaxOutlinksPerPage (graph.max_outlinks_per_page, default 500) caps a single
// page's stored out-degree (deterministic first-N) so a hostile 50k-anchor page
// cannot bloat the edges table. ExportMaxNodes / ExportMaxEdges (default 100 / 300)
// are the get_link_graph node/edge caps; the export further clamps each to a hard
// ceiling (250 / 750) regardless of config. These three are RESOURCE BOUNDS — a
// DoS surface — so they are deliberately NOT settable over the control plane (a
// caller must not be able to raise them): they stay file/env-only, mirroring
// crawler.hydration.max_payload_bytes and metrics.addr.
type GraphConfig struct {
	Enabled            bool   `koanf:"enabled"               yaml:"enabled"`
	SweepInterval      string `koanf:"sweep_interval"        yaml:"sweep_interval"`
	MaxOutlinksPerPage int    `koanf:"max_outlinks_per_page" yaml:"max_outlinks_per_page,omitempty"`
	ExportMaxNodes     int    `koanf:"export_max_nodes"      yaml:"export_max_nodes,omitempty"`
	ExportMaxEdges     int    `koanf:"export_max_edges"      yaml:"export_max_edges,omitempty"`
}

// MetricsConfig configures the read-only Prometheus /metrics listener (B2).
//
// Addr is the listen address (host:port). It is EMPTY BY DEFAULT — the metrics
// listener is off until a setup path enables it, at which point the value is the
// loopback default 127.0.0.1:9464 (the Prometheus exporter convention). The
// listener is unauthenticated and read-only, so binding it at all is a
// deliberate operator action, and binding it non-loopback logs a startup
// warning. metrics.addr is intentionally NOT settable over the control plane
// (allowlist.go): it changes network exposure and the listener binds only at
// startup (a reload does not rebind), so it is file/env-only.
type MetricsConfig struct {
	Addr string `koanf:"addr" yaml:"addr,omitempty"`
}

// SetupConfig records one-time onboarding state. AttestedAt is the RFC3339 UTC
// timestamp at which the operator attested authorization to monitor their sites.
type SetupConfig struct {
	AttestedAt string `koanf:"attested_at" yaml:"attested_at,omitempty"`
}

// RetentionConfig bounds database growth. The sweep runs every SweepInterval and:
//   - nulls raw_html on all but the newest RawHTMLKeep snapshots per URL (Layer 1);
//   - deletes change-less, non-latest snapshot rows older than SnapshotMaxAge
//     (Layer 2; SnapshotMaxAge ≤ 0 disables it — rows with recorded changes are
//     always kept, so history is never lost);
//   - trims robots/sitemap snapshots to the newest FileSnapshotsKeep per (site,kind).
//
// An absent section inherits these defaults (back-compatible with old config files).
type RetentionConfig struct {
	Enabled           bool   `koanf:"enabled"             yaml:"enabled"`
	SweepInterval     string `koanf:"sweep_interval"      yaml:"sweep_interval"`
	RawHTMLKeep       int    `koanf:"raw_html_keep"       yaml:"raw_html_keep"`
	SnapshotMaxAge    string `koanf:"snapshot_max_age"    yaml:"snapshot_max_age"`
	FileSnapshotsKeep int    `koanf:"file_snapshots_keep" yaml:"file_snapshots_keep"`
}

type ControlConfig struct {
	Port int `koanf:"port" yaml:"port"`
}

type LogConfig struct {
	Level string `koanf:"level" yaml:"level"`
	File  string `koanf:"file"  yaml:"file"`
}

type CrawlerConfig struct {
	UserAgent           string          `koanf:"user_agent"            yaml:"user_agent"`
	ContactEmail        string          `koanf:"contact_email"         yaml:"contact_email"`
	EgressCheckEndpoint string          `koanf:"egress_check_endpoint" yaml:"egress_check_endpoint"`
	EgressCheckEnabled  bool            `koanf:"egress_check_enabled"  yaml:"egress_check_enabled"`
	Hydration           HydrationConfig `koanf:"hydration"             yaml:"hydration,omitempty"`
}

// HydrationConfig controls A8 hydration-payload recovery during extraction.
//
// Enabled (crawler.hydration.enabled, default true) is the master switch: when
// false, extraction is byte-identical to pre-A8 — no payload back-fill, no payload
// prose in the content hash (render_mode is still classified for honesty). It is
// the ONE hydration key settable over the control plane (allowlist.go) so an agent
// can toggle recovery without a restart.
//
// MaxPayloadBytes (crawler.hydration.max_payload_bytes, default 2 MiB) is the
// per-payload decode cap handed to the decoders — a DoS guard against multi-MB
// embedded state. It is deliberately NOT settable over the control plane (it bounds
// resource use, mirroring how metrics.addr is file/env-only): a control-plane
// caller must not be able to raise the decode budget. A non-positive value disables
// the cap (the decoders' internal depth/node budgets still bound work).
type HydrationConfig struct {
	Enabled         bool `koanf:"enabled"           yaml:"enabled"`
	MaxPayloadBytes int  `koanf:"max_payload_bytes" yaml:"max_payload_bytes,omitempty"`
}

type DefaultsConfig struct {
	MinInterval        string `koanf:"min_interval"         yaml:"min_interval"`
	MaxInterval        string `koanf:"max_interval"         yaml:"max_interval"`
	PerHostConcurrency int    `koanf:"per_host_concurrency" yaml:"per_host_concurrency"`
	PerHostRate        string `koanf:"per_host_rate"        yaml:"per_host_rate"`
	// SpeedScale SEEDS a brand-new site's per-site speed dial at insert time
	// (reconcile.siteSpeedScale, default 100); it is NOT a global rate multiplier.
	// ResolveCrawl never consults defaults.speed_scale — only the per-site
	// SiteConfig.Speed scales the resolved per-host rate (scaleBySpeed; spec D2,
	// the per-site dial). Editing this key after a site exists does not change that
	// site's live rate. A global speed default consulted by ResolveCrawl is a
	// possible Spec B enhancement, deliberately not wired here (a product decision).
	SpeedScale         int                      `koanf:"speed_scale"          yaml:"speed_scale"`
	Discovery          DiscoveryConfig          `koanf:"discovery"            yaml:"discovery,omitempty"`
	UnverifiedThrottle UnverifiedThrottleConfig `koanf:"unverified_throttle"  yaml:"unverified_throttle,omitempty"`
}

// UnverifiedThrottleConfig is the SAFETY FLOOR applied to any site that is not
// proof-verified (the migration-default StateThrottled, an explicit attestation,
// or a legacy/empty record). It is an intentional default, not an operator
// convenience: a site whose control has not been proven crawls slower, with
// fewer concurrent connections and a smaller page budget, on a wider recheck
// cadence. The Phase 4 resolver (ResolveCrawl) composes these element-wise so
// the throttle can only ever SLOW/SHRINK a site below the full config tier,
// never speed it ABOVE that tier. A zeroed/blank field here can NOT silently
// void the floor — built-in fallback constants (throttle.go) backstop the
// zero/blank case. A deliberate POSITIVE override IS honored, though, and can
// loosen the floor toward the config base: the throttle is tunable by design, so
// the backstop defends only zero/blank, not an intentional operator config.
// Verification state is read from the authoritative DB proof record, never from
// config intent (spec D5).
type UnverifiedThrottleConfig struct {
	PerHostRate        string `koanf:"per_host_rate"        yaml:"per_host_rate,omitempty"`
	PerHostConcurrency int    `koanf:"per_host_concurrency" yaml:"per_host_concurrency,omitempty"`
	MaxPages           int    `koanf:"max_pages"            yaml:"max_pages,omitempty"`
	MinInterval        string `koanf:"min_interval"         yaml:"min_interval,omitempty"`
}

type SiteConfig struct {
	URL             string             `koanf:"url"              yaml:"url"`
	Name            string             `koanf:"name"             yaml:"name"`
	MinInterval     string             `koanf:"min_interval"     yaml:"min_interval,omitempty"`
	MaxInterval     string             `koanf:"max_interval"     yaml:"max_interval,omitempty"`
	Speed           int                `koanf:"speed"            yaml:"speed,omitempty"`
	ContentSelector string             `koanf:"content_selector" yaml:"content_selector,omitempty"`
	Segments        []SegmentConfig    `koanf:"segments"         yaml:"segments,omitempty"`
	Access          AccessConfig       `koanf:"access"           yaml:"access,omitempty"`
	Discovery       DiscoveryConfig    `koanf:"discovery"        yaml:"discovery,omitempty"`
	Verification    VerificationConfig `koanf:"verification"     yaml:"verification,omitempty"`
	GSC             GSCConfig          `koanf:"gsc"              yaml:"gsc,omitempty"`
}

// VerificationConfig is the per-site proof-of-control block in config.yaml. It
// records the operator's INTENT (which method + token they placed, and when it
// last verified) and is comment-preserving on write. It is NEVER trusted as
// proof on its own: the daemon re-verifies (Phase 4) and rewrites the
// authoritative living state in the DB. The token is public (placement is the
// proof), so it carries no secrecy requirement.
type VerificationConfig struct {
	Method     string `koanf:"method"      yaml:"method,omitempty"`
	Token      string `koanf:"token"       yaml:"token,omitempty"`
	VerifiedAt string `koanf:"verified_at" yaml:"verified_at,omitempty"`
}

type SegmentConfig struct {
	Name  string `koanf:"name"  yaml:"name"`
	Match string `koanf:"match" yaml:"match"`
}

type AccessConfig struct {
	Headers   map[string]string `koanf:"headers"    yaml:"headers,omitempty"`
	BasicUser string            `koanf:"basic_user" yaml:"basic_user,omitempty"`
	BasicPass string            `koanf:"basic_pass" yaml:"basic_pass,omitempty"`
	Cookies   map[string]string `koanf:"cookies"    yaml:"cookies,omitempty"`
	ProxyURL  string            `koanf:"proxy_url"  yaml:"proxy_url,omitempty"`
}

// NotifierConfig is one configured alert destination. Type selects the backend
// transport; the per-type fields below are optional on the struct and are required
// only for the type that consumes them (validated by ValidateNotifiers and again,
// hard, at daemon startup in supervisor.BuildAlertingStack):
//
//   - slack-webhook / generic-webhook: URL (+ Headers for generic-webhook).
//   - email-smtp: SMTPHost, SMTPPort, From, To (+ optional Username/Password,
//     AllowPlaintext).
//
// SECRET-BEARING FIELDS — Password, URL, and Headers values — may hold ${ENV}
// references that config.Load interpolates from the environment; they are NEVER
// logged, echoed into errors, or returned by `config get` (notifiers.* is denied
// from the control plane and absent from the get allow-list). Field order here is
// the yaml-tag write order used by AddNotifierYAML.
type NotifierConfig struct {
	Name string `koanf:"name" yaml:"name"`
	Type string `koanf:"type" yaml:"type"`
	URL  string `koanf:"url"  yaml:"url,omitempty"`

	// email-smtp fields.
	SMTPHost       string   `koanf:"smtp_host"       yaml:"smtp_host,omitempty"`
	SMTPPort       int      `koanf:"smtp_port"       yaml:"smtp_port,omitempty"`
	Username       string   `koanf:"username"        yaml:"username,omitempty"`
	Password       string   `koanf:"password"        yaml:"password,omitempty"` // secret: ${ENV}-interpolated, never logged
	From           string   `koanf:"from"            yaml:"from,omitempty"`
	To             []string `koanf:"to"              yaml:"to,omitempty"`
	AllowPlaintext bool     `koanf:"allow_plaintext" yaml:"allow_plaintext,omitempty"`

	// generic-webhook fields. Headers values are secrets (e.g. Authorization):
	// ${ENV}-interpolated by Load, never logged or echoed into errors.
	Headers map[string]string `koanf:"headers" yaml:"headers,omitempty"`
}

type RouteConfig struct {
	Match    map[string]string `koanf:"match"    yaml:"match"`
	Notifier string            `koanf:"notifier" yaml:"notifier"`
}

type AlertingConfig struct {
	DedupWindow            string       `koanf:"dedup_window"              yaml:"dedup_window"`
	PerRecipientHourlyCap  int          `koanf:"per_recipient_hourly_cap"  yaml:"per_recipient_hourly_cap"`
	IncidentAutoCloseAfter string       `koanf:"incident_auto_close_after" yaml:"incident_auto_close_after"`
	Digest                 DigestConfig `koanf:"digest"                    yaml:"digest"`
}

type DigestConfig struct {
	Schedule   string   `koanf:"schedule"   yaml:"schedule"`
	Severities []string `koanf:"severities" yaml:"severities"`
}

// DiscoveryConfig configures page discovery. Pointer fields distinguish
// "unset (inherit)" from an explicit value; the remaining zero ints/empty
// strings inherit. MaxPagesPerSite is a *int precisely so an explicit 0 can mean
// "unlimited" (the advertised `config set ...max_pages_per_site 0` remedy) rather
// than being mistaken for "unset" and falling back to the 2000 default.
type DiscoveryConfig struct {
	FollowLinks     *bool  `koanf:"follow_links"        yaml:"follow_links,omitempty"`
	Sitemap         *bool  `koanf:"sitemap"             yaml:"sitemap,omitempty"`
	MaxDepth        int    `koanf:"max_depth"           yaml:"max_depth,omitempty"`
	MaxPagesPerSite *int   `koanf:"max_pages_per_site"  yaml:"max_pages_per_site,omitempty"`
	SitemapRefresh  string `koanf:"sitemap_refresh"     yaml:"sitemap_refresh,omitempty"`
}

// DiscoveryResolved is DiscoveryConfig with defaults applied + durations parsed.
type DiscoveryResolved struct {
	FollowLinks    bool
	Sitemap        bool
	MaxDepth       int
	MaxPages       int
	SitemapRefresh time.Duration
}

// ResolveDiscovery merges per-site discovery config over the global defaults,
// returning a fully resolved DiscoveryResolved with all durations parsed.
func (c *Config) ResolveDiscovery(site SiteConfig) DiscoveryResolved {
	d, s := c.Defaults.Discovery, site.Discovery
	return DiscoveryResolved{
		FollowLinks:    boolOr(s.FollowLinks, d.FollowLinks, true),
		Sitemap:        boolOr(s.Sitemap, d.Sitemap, true),
		MaxDepth:       intOr(s.MaxDepth, d.MaxDepth, 3),
		MaxPages:       intPtrOr(s.MaxPagesPerSite, d.MaxPagesPerSite, 2000),
		SitemapRefresh: durOr(s.SitemapRefresh, d.SitemapRefresh, 24*time.Hour),
	}
}

func boolOr(site, def *bool, fallback bool) bool {
	if site != nil {
		return *site
	}
	if def != nil {
		return *def
	}
	return fallback
}

func intOr(site, def, fallback int) int {
	if site > 0 {
		return site
	}
	if def > 0 {
		return def
	}
	return fallback
}

// intPtrOr resolves a *int with inherit semantics, mirroring boolOr: a non-nil
// per-site value wins, else a non-nil default, else the fallback. Unlike intOr it
// preserves an explicit 0 (nil = inherit, &0 = the real value 0), which is what
// lets `max_pages_per_site: 0` mean "unlimited" instead of "unset".
func intPtrOr(site, def *int, fallback int) int {
	if site != nil {
		return *site
	}
	if def != nil {
		return *def
	}
	return fallback
}

// intPtr returns a pointer to v, for seeding *int config defaults/tests.
func intPtr(v int) *int { return &v }

func durOr(site, def string, fallback time.Duration) time.Duration {
	for _, v := range []string{site, def} {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

func defaultDiscovery() DiscoveryConfig {
	t := true
	return DiscoveryConfig{
		FollowLinks: &t, Sitemap: &t, MaxDepth: 3, MaxPagesPerSite: intPtr(2000), SitemapRefresh: "24h",
	}
}

// Defaults returns a Config populated with the built-in default values.
func Defaults() Config {
	return Config{
		Control: ControlConfig{Port: 7777},
		Log:     LogConfig{Level: "info", File: ""},
		Crawler: CrawlerConfig{
			EgressCheckEnabled:  true,
			EgressCheckEndpoint: "https://api.ipify.org",
			// A8: hydration recovery ON by default with a 2 MiB per-payload decode
			// cap. Defaults() seeds the struct, and Load merges file/env/flags OVER
			// these, so a config file that omits crawler.hydration inherits Enabled=true
			// rather than the koanf zero-value false.
			Hydration: HydrationConfig{
				Enabled:         true,
				MaxPayloadBytes: 2 * 1024 * 1024, // 2 MiB
			},
		},
		Defaults: DefaultsConfig{
			MinInterval:        "10m",
			MaxInterval:        "24h",
			PerHostConcurrency: 2,
			PerHostRate:        "2s",
			// Seeds a new site's per-site speed dial (see DefaultsConfig.SpeedScale);
			// NOT consulted by ResolveCrawl as a global rate multiplier.
			SpeedScale: 100,
			Discovery:  defaultDiscovery(),
			// Safety floor for unverified sites (spec D4/D5): polite 60s spacing,
			// single connection, a 50-page budget, and a 30m recheck cadence. The
			// throttle.go fallback constants backstop these so a zeroed config can
			// never void the floor.
			UnverifiedThrottle: UnverifiedThrottleConfig{
				PerHostRate:        "60s",
				PerHostConcurrency: 1,
				MaxPages:           50,
				MinInterval:        "30m",
			},
		},
		Alerting: AlertingConfig{
			DedupWindow:            "5m",
			PerRecipientHourlyCap:  30,
			IncidentAutoCloseAfter: "24h",
			Digest:                 DigestConfig{Schedule: "1h", Severities: []string{"info", "warning"}},
		},
		Retention: RetentionConfig{
			Enabled:           true,
			SweepInterval:     "6h",
			RawHTMLKeep:       1,
			SnapshotMaxAge:    "720h", // 30 days
			FileSnapshotsKeep: 10,
		},
		// A9 link-graph LITE on by default with the bounded caps. Defaults() seeds
		// the struct and Load merges file/env/flags OVER it, so a config file that
		// omits the `graph:` section inherits Enabled=true (not the koanf zero-value
		// false) — mirroring the A8 hydration default-true note above.
		Graph: GraphConfig{
			Enabled:            true,
			SweepInterval:      "6h",
			MaxOutlinksPerPage: 500,
			ExportMaxNodes:     100,
			ExportMaxEdges:     300,
		},
	}
}

// Validate checks structural constraints. crawler.contact_email is mandatory and
// must be a valid email address (the operator's published contact, surfaced in the
// crawler User-Agent so a site owner can reach whoever is crawling them).
func (c Config) Validate() error {
	if err := ValidateEmail(c.Crawler.ContactEmail); err != nil {
		return ErrContactEmailRequired
	}
	if c.Retention.Enabled {
		if c.Retention.RawHTMLKeep < 1 {
			return errors.New("rabbot: retention.raw_html_keep must be ≥ 1")
		}
		if c.Retention.FileSnapshotsKeep < 2 {
			return errors.New("rabbot: retention.file_snapshots_keep must be ≥ 2")
		}
		if d, err := time.ParseDuration(c.Retention.SweepInterval); err != nil || d <= 0 {
			return errors.New("rabbot: retention.sweep_interval must be a positive duration")
		}
		// snapshot_max_age ≤ 0 is valid (disables Layer 2); only reject an unparseable value.
		if _, err := time.ParseDuration(c.Retention.SnapshotMaxAge); err != nil {
			return errors.New("rabbot: retention.snapshot_max_age must be a valid duration")
		}
	}
	if err := ValidateNotifiers(c.Notifiers); err != nil {
		return err
	}
	if err := ValidateGSC(c.Sites); err != nil {
		return err
	}
	// metrics.addr is off (empty) by default; a non-empty value must be a
	// well-formed host:port the listener can bind. An empty value is valid (no
	// listener). net.SplitHostPort rejects a bare host with no port, an empty
	// port, or a malformed literal — the same parse the listener's net.Listen
	// would otherwise fail at startup, surfaced here as a clear config error.
	if c.Metrics.Addr != "" {
		if _, _, err := net.SplitHostPort(c.Metrics.Addr); err != nil {
			return errors.New("rabbot: metrics.addr must be a valid host:port (e.g. 127.0.0.1:9464)")
		}
	}
	if c.Graph.Enabled {
		if d, err := time.ParseDuration(c.Graph.SweepInterval); err != nil || d <= 0 {
			return errors.New("rabbot: graph.sweep_interval must be a positive duration")
		}
		// The caps are resource bounds; a negative value is a config error. Zero is
		// valid (the linkgraph constructor falls back to its package default), so
		// only reject an explicitly negative knob.
		if c.Graph.MaxOutlinksPerPage < 0 {
			return errors.New("rabbot: graph.max_outlinks_per_page must be >= 0")
		}
		if c.Graph.ExportMaxNodes < 0 {
			return errors.New("rabbot: graph.export_max_nodes must be >= 0")
		}
		if c.Graph.ExportMaxEdges < 0 {
			return errors.New("rabbot: graph.export_max_edges must be >= 0")
		}
	}
	return nil
}

// ValidateEmail is a lightweight, dependency-free email check: exactly one '@',
// a non-empty local part, a domain that contains a dot with non-empty labels, and
// no whitespace anywhere. It is intentionally permissive about the local part
// (RFC 5322 is far broader than any practical need here) but rejects the obvious
// mistakes — a bare URL, a hostname without a TLD, an empty side of the '@'. It is
// exported for reuse by the onboarding wizard's and headless setup's contact-email
// field so the accepted form never drifts from what Validate enforces.
func ValidateEmail(s string) error {
	if s == "" {
		return errors.New("config: email is empty")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return errors.New("config: email contains whitespace")
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at != strings.LastIndexByte(s, '@') {
		return errors.New("config: email must contain exactly one '@' with a non-empty local part")
	}
	domain := s[at+1:]
	if domain == "" {
		return errors.New("config: email domain is empty")
	}
	if !strings.Contains(domain, ".") {
		return errors.New("config: email domain has no dot")
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" {
			return errors.New("config: email domain has an empty label")
		}
	}
	return nil
}
