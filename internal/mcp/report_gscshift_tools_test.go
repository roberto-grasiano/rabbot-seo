package mcpsrv

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The summarize_changes handler computes the search_performance_shift annotation
// with time.Now() as the dataState=final clock, so the seeded metric days are built
// RELATIVE to real now: the change is `changeAgo` days back and the after-window is
// kept old enough (beyond the ~3-day partial lag) to be fully finalized regardless of
// when the test runs.

func ymd(t time.Time) string { return t.UTC().Format("2006-01-02") }

// TestSummarizeChanges_SurfacesSearchShift is the headline end-to-end for the MCP
// read path: a digest whose top URL has a change date + a SearchPerformance read with
// metrics spanning that date → the handler surfaces the search_performance_shift
// annotation with correct deltas. It proves the enrichment is reached from the
// production read path (Bridge.Report + Bridge.SearchPerformance), not the isolated fn.
func TestSummarizeChanges_SurfacesSearchShift(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	// Change 20 days ago so a full before window (days 27..21 ago) and a fully
	// finalized after window (days 19..13 ago, all > 3-day lag) exist around it.
	changeAt := now.AddDate(0, 0, -20)
	const u = "https://acme.test/p"

	var rows []SearchMetricView
	for d := 27; d >= 21; d-- { // before: strong
		rows = append(rows, SearchMetricView{Query: "widgets", Date: ymd(now.AddDate(0, 0, -d)), Impressions: 1000, Position: 4.0})
	}
	for d := 19; d >= 13; d-- { // after: collapsed (finalized)
		rows = append(rows, SearchMetricView{Query: "widgets", Date: ymd(now.AddDate(0, 0, -d)), Impressions: 200, Position: 9.0})
	}

	mb := &mockBridge{
		report: ReportView{
			Changes: ReportChangeSummary{Total: 1, Substantive: 1},
			TopURLs: []ReportURLChange{{URLID: 10, URL: u, Count: 1, LastChanged: changeAt.Format(time.RFC3339)}},
		},
		searchPerf: SearchPerformanceView{URL: u, HasData: true, Rows: rows},
	}

	h := summarizeChangesHandler(mb)
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, SummarizeChangesInput{})
	if err != nil {
		t.Fatalf("handler Go error: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("out.Error = %q, want empty", out.Error)
	}
	if len(out.SearchShifts) != 1 {
		t.Fatalf("want 1 search-shift annotation, got %d (%+v)", len(out.SearchShifts), out.SearchShifts)
	}
	got := out.SearchShifts[0]
	if got.URL != u {
		t.Errorf("annotation URL = %q, want %q", got.URL, u)
	}
	if got.Shift.Query != "widgets" {
		t.Errorf("primary query = %q, want widgets", got.Shift.Query)
	}
	if got.Shift.ImpressionsDelta >= 0 {
		t.Errorf("impressions delta = %d, want negative", got.Shift.ImpressionsDelta)
	}
	if got.Shift.PositionDelta <= 0 {
		t.Errorf("position delta = %.2f, want positive (worse)", got.Shift.PositionDelta)
	}
	if got.Annotation == "" {
		t.Errorf("annotation string must be rendered")
	}
	// The per-URL SearchPerformance read must have been bounded by a since the bridge saw.
	if mb.lastSearchPerfURL != u || mb.lastSearchPerfSince == "" {
		t.Errorf("SearchPerformance read = (url=%q since=%q), want the top URL + a bounded since", mb.lastSearchPerfURL, mb.lastSearchPerfSince)
	}
}

// TestSummarizeChanges_NoSearchData_NoAnnotation: the top URL has a change but the
// SearchPerformance read reports no data → no annotation, and the digest still
// returns cleanly (no fabrication, no error).
func TestSummarizeChanges_NoSearchData_NoAnnotation(t *testing.T) {
	t.Parallel()

	changeAt := time.Now().UTC().AddDate(0, 0, -20)
	mb := &mockBridge{
		report: ReportView{
			Changes: ReportChangeSummary{Total: 1, Substantive: 1},
			TopURLs: []ReportURLChange{{URLID: 10, URL: "https://acme.test/x", Count: 1, LastChanged: changeAt.Format(time.RFC3339)}},
		},
		searchPerf: SearchPerformanceView{URL: "https://acme.test/x", HasData: false},
	}

	h := summarizeChangesHandler(mb)
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, SummarizeChangesInput{})
	if err != nil {
		t.Fatalf("handler Go error: %v", err)
	}
	if len(out.SearchShifts) != 0 {
		t.Fatalf("no GSC data must attach NO annotation, got %+v", out.SearchShifts)
	}
	if out.Report.Changes.Total != 1 {
		t.Fatalf("digest must still surface, got %+v", out.Report.Changes)
	}
}

// TestSummarizeChanges_PartialAfterData_NoAnnotation: metrics exist but the only
// post-change days are within the partial lag → SearchPerformanceShift returns no
// enrichment, so the handler attaches nothing.
func TestSummarizeChanges_PartialAfterData_NoAnnotation(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	// Change yesterday: any "after" day is within the ~3-day partial lag.
	changeAt := now.AddDate(0, 0, -1)
	const u = "https://acme.test/recent"
	var rows []SearchMetricView
	for d := 8; d >= 2; d-- { // before window (finalized) only
		rows = append(rows, SearchMetricView{Query: "widgets", Date: ymd(now.AddDate(0, 0, -d)), Impressions: 1000, Position: 4.0})
	}
	// today: a partial after-day.
	rows = append(rows, SearchMetricView{Query: "widgets", Date: ymd(now), Impressions: 50, Position: 9.0})

	mb := &mockBridge{
		report: ReportView{
			Changes: ReportChangeSummary{Total: 1, Substantive: 1},
			TopURLs: []ReportURLChange{{URLID: 10, URL: u, Count: 1, LastChanged: changeAt.Format(time.RFC3339)}},
		},
		searchPerf: SearchPerformanceView{URL: u, HasData: true, Rows: rows},
	}

	h := summarizeChangesHandler(mb)
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, SummarizeChangesInput{})
	if err != nil {
		t.Fatalf("handler Go error: %v", err)
	}
	if len(out.SearchShifts) != 0 {
		t.Fatalf("partial-only after-window must attach NO annotation, got %+v", out.SearchShifts)
	}
}

// TestSummarizeChanges_DaemonDown_NoShiftPanic: a down daemon on Report is
// errors-as-data and never reaches the shift annotation (no SearchPerformance call).
func TestSummarizeChanges_DaemonDown_NoShiftPanic(t *testing.T) {
	t.Parallel()

	mb := &mockBridge{reportErr: errDaemonDownForTest{}}
	h := summarizeChangesHandler(mb)
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, SummarizeChangesInput{})
	if err != nil {
		t.Fatalf("daemon down must be data, got Go error: %v", err)
	}
	if out.Error == "" {
		t.Fatalf("want a friendly error in the payload")
	}
	if len(out.SearchShifts) != 0 {
		t.Fatalf("no annotation on a down daemon, got %+v", out.SearchShifts)
	}
}

// errDaemonDownForTest is a minimal error to drive the Report-down branch.
type errDaemonDownForTest struct{}

func (errDaemonDownForTest) Error() string { return "daemon not running" }
