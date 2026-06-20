package mcpsrv

import (
	"context"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// TestGetRichResultsHandler_OK asserts get_rich_results against a mock Bridge
// returns the per-type eligibility DTO and passes the URL through.
func TestGetRichResultsHandler_OK(t *testing.T) {
	t.Parallel()
	m := &mockBridge{richResults: RichResultsView{
		URL:         "https://shop.test/p",
		HasSnapshot: true,
		Profile:     "grr-2026.06",
		Entities: []RichResultEntityView{
			{Type: "Product", RawType: "Product", Eligible: false, Missing: []string{"name"}, MissingAnyOf: [][]string{{"offers", "review"}}},
			{Type: "Article", RawType: "BlogPosting", Eligible: true},
		},
		Unprofiled: 1,
	}}
	h := getRichResultsHandler(m)
	_, out, err := h(context.Background(), nil, GetRichResultsInput{URL: "https://shop.test/p"})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if m.lastRichResultsURL != "https://shop.test/p" {
		t.Fatalf("url = %q, want https://shop.test/p", m.lastRichResultsURL)
	}
	if out.Error != "" {
		t.Fatalf("unexpected error: %q", out.Error)
	}
	rr := out.RichResults
	if rr.Profile != "grr-2026.06" || !rr.HasSnapshot || rr.Unprofiled != 1 || len(rr.Entities) != 2 {
		t.Fatalf("rich-results out = %+v", rr)
	}
	if rr.Entities[0].Type != "Product" || rr.Entities[0].Eligible ||
		len(rr.Entities[0].Missing) != 1 || len(rr.Entities[0].MissingAnyOf) != 1 {
		t.Fatalf("entity[0] = %+v", rr.Entities[0])
	}
	if rr.Entities[1].RawType != "BlogPosting" || !rr.Entities[1].Eligible {
		t.Fatalf("entity[1] = %+v", rr.Entities[1])
	}
}

// TestGetRichResultsHandler_NotFound asserts an unknown URL is reported as data
// (RichResultsView.NotFound=true), not a Go error.
func TestGetRichResultsHandler_NotFound(t *testing.T) {
	t.Parallel()
	m := &mockBridge{richResults: RichResultsView{URL: "https://x/missing", NotFound: true}}
	h := getRichResultsHandler(m)
	_, out, err := h(context.Background(), nil, GetRichResultsInput{URL: "https://x/missing"})
	if err != nil {
		t.Fatalf("not-found must be data, not error: %v", err)
	}
	if !out.RichResults.NotFound || out.Error != "" {
		t.Fatalf("want not-found-as-data, got %+v", out)
	}
}

// TestGetRichResultsHandler_NoSnapshot asserts a monitored-but-uncrawled URL is
// reported with has_snapshot=false and the profile still set — not not-found.
func TestGetRichResultsHandler_NoSnapshot(t *testing.T) {
	t.Parallel()
	m := &mockBridge{richResults: RichResultsView{URL: "https://x/fresh", HasSnapshot: false, Profile: "grr-2026.06", Entities: []RichResultEntityView{}}}
	h := getRichResultsHandler(m)
	_, out, err := h(context.Background(), nil, GetRichResultsInput{URL: "https://x/fresh"})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if out.RichResults.NotFound {
		t.Fatalf("uncrawled URL must not be not-found: %+v", out)
	}
	if out.RichResults.HasSnapshot || out.RichResults.Profile == "" {
		t.Fatalf("want has_snapshot=false + profile set, got %+v", out.RichResults)
	}
}

// TestGetRichResultsHandler_DaemonDown asserts a down daemon is errors-as-data,
// never a tool crash.
func TestGetRichResultsHandler_DaemonDown(t *testing.T) {
	t.Parallel()
	m := &mockBridge{richResultsErr: control.ErrDaemonNotRunning}
	h := getRichResultsHandler(m)
	_, out, err := h(context.Background(), nil, GetRichResultsInput{URL: "https://x/p"})
	if err != nil {
		t.Fatalf("daemon-down must be data, not error: %v", err)
	}
	if out.Error == "" {
		t.Fatalf("expected errors-as-data Error field")
	}
}
