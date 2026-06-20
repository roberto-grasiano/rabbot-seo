package notify

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

type recordingNotifier struct {
	name string
	got  []Alert
}

func (r *recordingNotifier) Name() string { return r.name }
func (r *recordingNotifier) Notify(ctx context.Context, a Alert) error {
	r.got = append(r.got, a)
	return nil
}

func TestRegistryRouteBySeverity(t *testing.T) {
	crit := &recordingNotifier{name: "slack-critical"}
	digest := &recordingNotifier{name: "slack-digest"}
	routes := []config.RouteConfig{
		{Match: map[string]string{"severity": "critical"}, Notifier: "slack-critical"},
		{Match: map[string]string{}, Notifier: "slack-digest"}, // fallback
	}
	reg := NewRegistry(map[string]Notifier{"slack-critical": crit, "slack-digest": digest}, routes)

	got := reg.Route(Alert{Site: "x", Severity: model.SeverityCritical})
	if len(got) != 1 || got[0].Name() != "slack-critical" {
		t.Fatalf("critical should route to slack-critical, got %v", names(got))
	}
	got = reg.Route(Alert{Site: "x", Severity: model.SeverityInfo})
	if len(got) != 1 || got[0].Name() != "slack-digest" {
		t.Fatalf("info should fall back to slack-digest, got %v", names(got))
	}
}

func TestRegistryRouteBySite(t *testing.T) {
	a := &recordingNotifier{name: "a"}
	b := &recordingNotifier{name: "b"}
	routes := []config.RouteConfig{
		{Match: map[string]string{"site": "example.com"}, Notifier: "a"},
		{Match: map[string]string{}, Notifier: "b"},
	}
	reg := NewRegistry(map[string]Notifier{"a": a, "b": b}, routes)
	got := reg.Route(Alert{Site: "example.com", Severity: model.SeverityWarning})
	if len(got) != 1 || got[0].Name() != "a" {
		t.Errorf("site match failed, got %v", names(got))
	}
}

func TestRegistryRouteBySegment(t *testing.T) {
	a := &recordingNotifier{name: "slack-content"}
	b := &recordingNotifier{name: "slack-digest"}
	routes := []config.RouteConfig{
		{Match: map[string]string{"segment": "content"}, Notifier: "slack-content"},
		{Match: map[string]string{}, Notifier: "slack-digest"}, // fallback
	}
	reg := NewRegistry(map[string]Notifier{"slack-content": a, "slack-digest": b}, routes)

	// An alert in the content segment routes to slack-content.
	got := reg.Route(Alert{Site: "x", URL: "https://x/blog/p", Severity: model.SeverityWarning, Segments: []string{"content"}})
	if len(got) != 1 || got[0].Name() != "slack-content" {
		t.Fatalf("content-segment alert should route to slack-content, got %v", names(got))
	}
	// An alert in no segment falls through to the fallback.
	got = reg.Route(Alert{Site: "x", URL: "https://x/other", Severity: model.SeverityWarning})
	if len(got) != 1 || got[0].Name() != "slack-digest" {
		t.Fatalf("no-segment alert should fall back to slack-digest, got %v", names(got))
	}
	// A site-level event (no URL, no segments) falls through to the fallback.
	got = reg.Route(Alert{Site: "x", ChangeType: "robots_txt", Severity: model.SeverityCritical})
	if len(got) != 1 || got[0].Name() != "slack-digest" {
		t.Fatalf("site-level event should fall back to slack-digest, got %v", names(got))
	}
	// Any-of semantics: an alert in two segments matches a route on either one.
	got = reg.Route(Alert{Site: "x", URL: "https://x/blog/featured/p", Severity: model.SeverityWarning, Segments: []string{"featured", "content"}})
	if len(got) != 1 || got[0].Name() != "slack-content" {
		t.Fatalf("any-of: alert in two segments should match a route on either, got %v", names(got))
	}
}

func TestRouteMatchesSegmentUnknownNeverMatches(t *testing.T) {
	// An unknown segment value on the alert must not match a segment route.
	if routeMatches(map[string]string{"segment": "content"}, Alert{Segments: []string{"product"}}) {
		t.Error("a route for segment=content must not match an alert in segment=product")
	}
	// An empty segments list never matches a segment route.
	if routeMatches(map[string]string{"segment": "content"}, Alert{}) {
		t.Error("a route for segment=content must not match an alert with no segments")
	}
	// An unknown match KEY still never matches (regression: the new case must not
	// have broken the default).
	if routeMatches(map[string]string{"bogus": "x"}, Alert{Segments: []string{"content"}}) {
		t.Error("an unknown match key must never match")
	}
}

func TestDispatchFansOut(t *testing.T) {
	crit := &recordingNotifier{name: "slack-critical"}
	routes := []config.RouteConfig{{Match: map[string]string{}, Notifier: "slack-critical"}}
	reg := NewRegistry(map[string]Notifier{"slack-critical": crit}, routes)
	d := NewDispatcher(reg)
	if err := d.Dispatch(context.Background(), Alert{Site: "x", Severity: model.SeverityCritical, DetectedAt: time.Now()}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(crit.got) != 1 {
		t.Errorf("expected 1 delivery, got %d", len(crit.got))
	}
}

func TestRouteTargetAndThrottleKey(t *testing.T) {
	crit := &recordingNotifier{name: "slack-critical"}
	digest := &recordingNotifier{name: "slack-digest"}
	routes := []config.RouteConfig{
		{Match: map[string]string{"severity": "critical"}, Notifier: "slack-critical"},
		{Match: map[string]string{}, Notifier: "slack-digest"}, // fallback
	}
	reg := NewRegistry(map[string]Notifier{"slack-critical": crit, "slack-digest": digest}, routes)

	// RouteTarget exposes the routed channel name (first-match-wins).
	if name, ok := reg.RouteTarget(Alert{Severity: model.SeverityCritical}); !ok || name != "slack-critical" {
		t.Fatalf("RouteTarget(critical) = %q,%v; want slack-critical,true", name, ok)
	}
	if name, ok := reg.RouteTarget(Alert{Severity: model.SeverityInfo}); !ok || name != "slack-digest" {
		t.Fatalf("RouteTarget(info) = %q,%v; want slack-digest,true (fallback)", name, ok)
	}

	// Dispatcher.ThrottleKey delegates to RouteTarget so the pipeline throttle is keyed
	// per destination channel: two different sites at critical share one throttle key.
	d := NewDispatcher(reg)
	k1, ok1 := d.ThrottleKey(Alert{Site: "a.com", Severity: model.SeverityCritical})
	k2, ok2 := d.ThrottleKey(Alert{Site: "b.com", Severity: model.SeverityCritical})
	if !ok1 || !ok2 || k1 != "slack-critical" || k1 != k2 {
		t.Fatalf("ThrottleKey should be the shared channel for both sites; got %q,%v and %q,%v", k1, ok1, k2, ok2)
	}
}

func names(ns []Notifier) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Name()
	}
	return out
}
