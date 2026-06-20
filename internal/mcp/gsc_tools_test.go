package mcpsrv

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// ─── get_index_status ────────────────────────────────────────────────────────

func TestToolGetIndexStatus(t *testing.T) {
	t.Parallel()

	want := IndexStatusView{
		URL:             "https://acme.test/p",
		HasStatus:       true,
		Verdict:         "PASS",
		CoverageState:   "Submitted and indexed",
		IndexingState:   "INDEXING_ALLOWED",
		GoogleCanonical: "https://acme.test/p",
		UserCanonical:   "https://acme.test/p",
		InspectedAt:     "2026-06-18T00:00:00Z",
		LastCrawlTime:   "2026-06-17T00:00:00Z",
	}
	mb := &mockBridge{indexStatus: want}
	h := getIndexStatusHandler(mb)
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetIndexStatusInput{URL: "https://acme.test/p"})
	if err != nil {
		t.Fatalf("handler Go error: %v", err)
	}
	if !reflect.DeepEqual(out.IndexStatus, want) {
		t.Fatalf("out.IndexStatus = %+v, want %+v", out.IndexStatus, want)
	}
	if out.Error != "" {
		t.Fatalf("out.Error = %q, want empty", out.Error)
	}
	if mb.lastIndexStatusURL != "https://acme.test/p" {
		t.Fatalf("url = %q, want the input url", mb.lastIndexStatusURL)
	}
}

// TestToolGetIndexStatus_NotFoundIsData pins the central GSC W2 invariant: an
// un-inspected URL (quota-bounded staleness) is reported as has_status=false /
// not_found=true IN the payload — never a Go error and NEVER a discrepancy.
func TestToolGetIndexStatus_NotFoundIsData(t *testing.T) {
	t.Parallel()

	h := getIndexStatusHandler(&mockBridge{indexStatus: IndexStatusView{URL: "https://acme.test/x", NotFound: true}})
	res, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetIndexStatusInput{URL: "https://acme.test/x"})
	if err != nil {
		t.Fatalf("not-found must not be a Go error, got: %v", err)
	}
	if res != nil {
		t.Fatalf("res = %+v, want nil", res)
	}
	if !out.IndexStatus.NotFound || out.IndexStatus.HasStatus {
		t.Fatalf("want NotFound=true HasStatus=false, got %+v", out.IndexStatus)
	}
	if out.Error != "" {
		t.Fatalf("out.Error = %q, want empty (not-found is data)", out.Error)
	}
}

// TestIndexStatusHasStatusFalseSerializes pins that a genuine has_status=false is
// present in the JSON (no omitempty), so a consumer distinguishes "no GSC data on
// record" from "field absent".
func TestIndexStatusHasStatusFalseSerializes(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(IndexStatusView{URL: "https://x.test", NotFound: true, HasStatus: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"has_status":false`) {
		t.Fatalf("has_status=false was dropped from JSON: %s", raw)
	}
}

func TestToolGetIndexStatus_DaemonDown(t *testing.T) {
	t.Parallel()

	h := getIndexStatusHandler(&mockBridge{indexStatusErr: control.ErrDaemonNotRunning})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetIndexStatusInput{URL: "https://x.test"})
	if err != nil {
		t.Fatalf("daemon down must not be a Go error, got: %v", err)
	}
	if !strings.Contains(out.Error, "daemon not running") {
		t.Fatalf("out.Error = %q, want a daemon-not-running message", out.Error)
	}
}

// ─── get_search_performance ──────────────────────────────────────────────────

func TestToolGetSearchPerformance(t *testing.T) {
	t.Parallel()

	want := SearchPerformanceView{
		URL:     "https://acme.test/p",
		HasData: true,
		Rows: []SearchMetricView{
			{Query: "q1", Date: "2026-06-15", Clicks: 10, Impressions: 100, CTR: 0.1, Position: 4.2},
		},
	}
	mb := &mockBridge{searchPerf: want}
	h := getSearchPerformanceHandler(mb)
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetSearchPerformanceInput{URL: "https://acme.test/p"})
	if err != nil {
		t.Fatalf("handler Go error: %v", err)
	}
	if !reflect.DeepEqual(out.SearchPerformance, want) {
		t.Fatalf("out.SearchPerformance = %+v, want %+v", out.SearchPerformance, want)
	}
	// Empty since => default window applied; the bridge sees a non-empty RFC3339 since.
	if mb.lastSearchPerfSince == "" {
		t.Fatalf("since = empty, want a defaulted RFC3339 lower bound")
	}
	if mb.lastSearchPerfURL != "https://acme.test/p" {
		t.Fatalf("url = %q, want the input url", mb.lastSearchPerfURL)
	}
}

// TestToolGetSearchPerformance_SinceParsed asserts an explicit Go-duration since is
// resolved to an absolute RFC3339 lower bound passed to the bridge.
func TestToolGetSearchPerformance_SinceParsed(t *testing.T) {
	t.Parallel()

	mb := &mockBridge{}
	h := getSearchPerformanceHandler(mb)
	before := time.Now().UTC().Add(-49 * time.Hour)
	_, _, err := h(context.Background(), &mcp.CallToolRequest{}, GetSearchPerformanceInput{URL: "https://x.test", Since: "48h"})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	got, perr := time.Parse(time.RFC3339, mb.lastSearchPerfSince)
	if perr != nil {
		t.Fatalf("since not RFC3339: %q (%v)", mb.lastSearchPerfSince, perr)
	}
	after := time.Now().UTC().Add(-47 * time.Hour)
	if got.Before(before) || got.After(after) {
		t.Fatalf("since = %v, want ~48h ago", got)
	}
}

// TestToolGetSearchPerformance_BadSinceIsToolError asserts a malformed since is a
// tool error the model can correct (the get_health_score / summarize_changes
// contract), distinct from daemon-down/no-data which are data.
func TestToolGetSearchPerformance_BadSinceIsToolError(t *testing.T) {
	t.Parallel()

	h := getSearchPerformanceHandler(&mockBridge{})
	res, _, err := h(context.Background(), &mcp.CallToolRequest{}, GetSearchPerformanceInput{URL: "https://x.test", Since: "lastweek"})
	if err == nil {
		t.Fatalf("malformed since must be a tool error, got nil")
	}
	if res != nil {
		t.Fatalf("res = %+v, want nil (error carried via Go error)", res)
	}
}

// TestToolGetSearchPerformance_NoDataIsData asserts a URL with no metrics is
// reported as has_data=false in the payload — never an error (the quota honesty).
func TestToolGetSearchPerformance_NoDataIsData(t *testing.T) {
	t.Parallel()

	h := getSearchPerformanceHandler(&mockBridge{searchPerf: SearchPerformanceView{URL: "https://x.test"}})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetSearchPerformanceInput{URL: "https://x.test"})
	if err != nil {
		t.Fatalf("no-data must not be a Go error, got: %v", err)
	}
	if out.SearchPerformance.HasData {
		t.Fatalf("want HasData=false, got %+v", out.SearchPerformance)
	}
	if out.Error != "" {
		t.Fatalf("out.Error = %q, want empty", out.Error)
	}
}

func TestToolGetSearchPerformance_DaemonDown(t *testing.T) {
	t.Parallel()

	h := getSearchPerformanceHandler(&mockBridge{searchPerfErr: control.ErrDaemonNotRunning})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetSearchPerformanceInput{URL: "https://x.test"})
	if err != nil {
		t.Fatalf("daemon down must not be a Go error, got: %v", err)
	}
	if !strings.Contains(out.Error, "daemon not running") {
		t.Fatalf("out.Error = %q, want a daemon-not-running message", out.Error)
	}
}

// TestSearchPerformanceRowsNormalizeToEmpty asserts the Rows slice is non-nil so
// the JSON is [] not null even when the URL has no metrics.
func TestSearchPerformanceRowsNormalizeToEmpty(t *testing.T) {
	t.Parallel()

	h := getSearchPerformanceHandler(&mockBridge{searchPerf: SearchPerformanceView{URL: "https://x.test", Rows: nil}})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetSearchPerformanceInput{URL: "https://x.test"})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if out.SearchPerformance.Rows == nil {
		t.Fatalf("Rows = nil, want non-nil empty slice")
	}
}
