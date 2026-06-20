package mcpsrv

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// TestGetHealthScoreHandler_Happy asserts the get_health_score tool returns the
// live score + coverage + series, and threads site_id/segment/since to the bridge.
func TestGetHealthScoreHandler_Happy(t *testing.T) {
	t.Parallel()
	m := &mockBridge{score: ScoreView{
		SiteID: 7, Defined: true, Score: 87.5, MaxMass: 1000, KnownURLs: 10, ProcessedURLs: 8,
		PageCount: 8, OpenCritical: 1, Breakdown: `{"title-missing":125}`,
		Series: []ScorePointView{{ComputedAt: "2026-06-11T00:00:00Z", Score: 87.5, MaxMass: 1000, PageCount: 8}},
	}}
	h := getHealthScoreHandler(m)

	_, out, err := h(context.Background(), nil, GetHealthScoreInput{SiteID: 7, Segment: "content", Since: "24h"})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected error field: %q", out.Error)
	}
	if !out.Health.Defined || out.Health.Score != 87.5 || out.Health.KnownURLs != 10 || out.Health.ProcessedURLs != 8 {
		t.Fatalf("health = %+v", out.Health)
	}
	if len(out.Health.Series) != 1 {
		t.Fatalf("series = %v, want 1 point", out.Health.Series)
	}
	// Threaded args.
	if m.lastHealthQuery.SiteID != 7 || m.lastHealthQuery.Segment != "content" {
		t.Fatalf("query = %+v", m.lastHealthQuery)
	}
	if d := time.Since(m.lastHealthQuery.Since); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("since window = %v, want ~24h", d)
	}
}

// TestGetHealthScoreHandler_DefaultWindow asserts the default `since` is 168h (7d),
// same contract as summarize_changes.
func TestGetHealthScoreHandler_DefaultWindow(t *testing.T) {
	t.Parallel()
	m := &mockBridge{}
	h := getHealthScoreHandler(m)
	if _, _, err := h(context.Background(), nil, GetHealthScoreInput{SiteID: 1}); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if d := time.Since(m.lastHealthQuery.Since); d < 167*time.Hour || d > 169*time.Hour {
		t.Fatalf("default window = %v, want ~168h", d)
	}
}

// TestGetHealthScoreHandler_NotFound asserts an unknown site/segment is errors-as-data
// (Health.NotFound=true), NOT a tool error.
func TestGetHealthScoreHandler_NotFound(t *testing.T) {
	t.Parallel()
	m := &mockBridge{score: ScoreView{NotFound: true}}
	h := getHealthScoreHandler(m)
	_, out, err := h(context.Background(), nil, GetHealthScoreInput{SiteID: 999})
	if err != nil {
		t.Fatalf("unknown site must be data, not tool error: %v", err)
	}
	if !out.Health.NotFound || out.Error != "" {
		t.Fatalf("expected NotFound data, got %+v err=%q", out.Health, out.Error)
	}
}

// TestGetHealthScoreHandler_BadSince asserts a malformed duration is a tool error
// (the model can correct it).
func TestGetHealthScoreHandler_BadSince(t *testing.T) {
	t.Parallel()
	h := getHealthScoreHandler(&mockBridge{})
	if _, _, err := h(context.Background(), nil, GetHealthScoreInput{SiteID: 1, Since: "banana"}); err == nil {
		t.Fatalf("expected tool error for malformed since duration")
	}
}

// TestGetHealthScoreHandler_DaemonDown asserts a down daemon is errors-as-data.
func TestGetHealthScoreHandler_DaemonDown(t *testing.T) {
	t.Parallel()
	m := &mockBridge{scoreErr: control.ErrDaemonNotRunning}
	h := getHealthScoreHandler(m)
	_, out, err := h(context.Background(), nil, GetHealthScoreInput{SiteID: 1})
	if err != nil {
		t.Fatalf("daemon-down must be data, not error: %v", err)
	}
	if out.Error == "" {
		t.Fatalf("expected errors-as-data Error field")
	}
}
