// Package notify defines the backend-agnostic Notifier interface, a route-aware
// registry/dispatcher, and the Slack Incoming-Webhook backend.
package notify

import (
	"context"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// Alert is the backend-agnostic payload produced by the alerts pipeline.
type Alert struct {
	Site         string
	URL          string // empty for site-level (robots/sitemap/operational)
	ChangeType   string // e.g. "indexability","robots_txt","title","monitoring_blocked"
	Severity     model.Severity
	Before       string
	After        string
	DetectedAt   time.Time
	GroupKey     string // site+change_type
	RelatedCount int    // rolled-up affected pages
	DeepLink     string // affected URL, or a `rabbot history <url>` CLI hint
	Operational  bool   // true for monitoring_blocked/monitoring_unreachable (access incidents)
	Items        []AlertItem
	// Segments is the set of segment names the alert's URL belongs to (A7). It is
	// copied from the originating alerts.Event at emit time and consumed by
	// route matching (match:{segment: <name>}). Empty for site-level events
	// (robots/sitemap/operational, no URL), so a segment route never matches them.
	Segments []string
}

// AlertItem is one rolled-up affected page in a grouped alert.
type AlertItem struct {
	URL    string
	Before string
	After  string
}

// Notifier delivers an Alert to one configured destination.
type Notifier interface {
	Name() string
	Notify(ctx context.Context, a Alert) error
}

// Registry resolves config routes to notifiers and fans out.
type Registry interface {
	Get(name string) (Notifier, bool)
	Route(a Alert) []Notifier // resolves config routes (match{site,severity}) -> notifiers
	// RouteTarget returns the name of the notifier the first matching route would
	// deliver a to (and whether a route matched), without delivering — used to key
	// the alert throttle by the destination channel rather than by site+severity.
	RouteTarget(a Alert) (string, bool)
}
