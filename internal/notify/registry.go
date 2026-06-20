package notify

import (
	"context"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

// registry implements Registry over a fixed notifier map + ordered routes.
type registry struct {
	byName map[string]Notifier
	routes []config.RouteConfig
}

// NewRegistry builds a Registry. Routes are evaluated in order; the first
// matching route wins. An empty match map ({}) is the fallback.
func NewRegistry(byName map[string]Notifier, routes []config.RouteConfig) Registry {
	return &registry{byName: byName, routes: routes}
}

func (r *registry) Get(name string) (Notifier, bool) {
	n, ok := r.byName[name]
	return n, ok
}

// Route returns the notifiers for the first matching route. A route matches when
// every key in its Match map equals the alert's corresponding attribute. An
// empty Match map matches everything (fallback).
func (r *registry) Route(a Alert) []Notifier {
	for _, route := range r.routes {
		if routeMatches(route.Match, a) {
			if n, ok := r.byName[route.Notifier]; ok {
				return []Notifier{n}
			}
		}
	}
	return nil
}

// RouteTarget returns the name of the notifier the first matching route resolves
// to (mirroring Route's first-match-wins logic) and whether a route matched,
// without delivering. It exposes the destination identity so the alert pipeline
// can key its per-recipient throttle by the actual channel.
func (r *registry) RouteTarget(a Alert) (string, bool) {
	for _, route := range r.routes {
		if routeMatches(route.Match, a) {
			if _, ok := r.byName[route.Notifier]; ok {
				return route.Notifier, true
			}
		}
	}
	return "", false
}

func routeMatches(match map[string]string, a Alert) bool {
	for k, v := range match {
		switch k {
		case "severity":
			if string(a.Severity) != v {
				return false
			}
		case "site":
			if a.Site != v {
				return false
			}
		case "change_type":
			if a.ChangeType != v {
				return false
			}
		case "segment":
			// Any-of: the route matches iff ANY of the alert's segments equals the
			// configured value. An alert with no segments (site-level events, or a
			// URL in no segment) never matches a segment route. Unknown segment
			// values simply never match — they don't error.
			if !containsString(a.Segments, v) {
				return false
			}
		default:
			return false // unknown match key never matches
		}
	}
	return true
}

// containsString reports whether s contains v. Used by the segment route case
// for any-of matching against the alert's segment names.
func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Dispatcher resolves an alert's route and delivers it to the resolved
// notifier(s). Routing is first-match-wins (see Registry.Route): a given alert
// resolves to exactly one matching route, so an alert is delivered to that
// route's single notifier — it is NOT broadcast across every route whose Match
// happens to apply.
type Dispatcher struct {
	reg     Registry
	metrics *obs.Metrics // nil-safe self-observability; never wired to a webhook URL
}

// DispatcherOption configures a Dispatcher at construction.
type DispatcherOption func(*Dispatcher)

// WithMetrics installs the self-observability layer that records one
// rabbot_alerts_dispatched_total sample per delivered notifier. The notifier
// label is the operator-config name (Notifier.Name()), NEVER the webhook URL; a
// nil *Metrics no-ops, so this is safe to omit. This is the single delivery
// funnel, so every push channel (Slack/email/webhook, pipeline + digest flush)
// inherits dispatch metrics for free.
func WithMetrics(m *obs.Metrics) DispatcherOption {
	return func(d *Dispatcher) { d.metrics = m }
}

// NewDispatcher builds a Dispatcher over a Registry.
func NewDispatcher(reg Registry, opts ...DispatcherOption) *Dispatcher {
	d := &Dispatcher{reg: reg}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Dispatch delivers a to each notifier returned by the registry's first matching
// route; it returns the first delivery error but always attempts every notifier
// in that result.
func (d *Dispatcher) Dispatch(ctx context.Context, a Alert) error {
	var firstErr error
	for _, n := range d.reg.Route(a) {
		err := n.Notify(ctx, a)
		// Record the outcome of THIS delivery, labelled by the notifier's
		// operator-config name (never the webhook URL or the error string). The
		// error is passed only so ObserveDispatch can map it to the closed
		// outcome enum {ok,error}.
		d.metrics.ObserveDispatch(n.Name(), err)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ThrottleKey exposes the routed delivery-target name for a, satisfying the alerts
// pipeline's optional throttle-keyer seam so the per-recipient hourly cap is keyed by
// the destination channel: many sites funneling into one notifier share one bucket.
// ok is false when no route matches, and the caller falls back to its own key.
func (d *Dispatcher) ThrottleKey(a Alert) (string, bool) {
	return d.reg.RouteTarget(a)
}
