package mcpsrv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// TestToolListIssues_SegmentPassthrough asserts the list_issues tool threads the
// optional segment name onto the IssueQuery the bridge receives (A7).
func TestToolListIssues_SegmentPassthrough(t *testing.T) {
	t.Parallel()
	mb := &mockBridge{}
	h := listIssuesHandler(mb)

	if _, _, err := h(context.Background(), &mcp.CallToolRequest{}, ListIssuesInput{Segment: "content"}); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if mb.lastIssueQuery.Segment != "content" {
		t.Fatalf("bridge saw segment %q, want content", mb.lastIssueQuery.Segment)
	}

	// Empty segment stays empty (no filter).
	mb.lastIssueQuery = IssueQuery{}
	if _, _, err := h(context.Background(), &mcp.CallToolRequest{}, ListIssuesInput{}); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if mb.lastIssueQuery.Segment != "" {
		t.Fatalf("absent segment became %q, want empty", mb.lastIssueQuery.Segment)
	}
}

// TestToolSummarizeChanges_SegmentPassthrough asserts summarize_changes threads the
// optional segment name onto the ReportQuery the bridge receives (A7).
func TestToolSummarizeChanges_SegmentPassthrough(t *testing.T) {
	t.Parallel()
	m := &mockBridge{}
	h := summarizeChangesHandler(m)

	if _, _, err := h(context.Background(), nil, SummarizeChangesInput{Segment: "product"}); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if m.lastReportQuery.Segment != "product" {
		t.Fatalf("bridge saw segment %q, want product", m.lastReportQuery.Segment)
	}
}

// TestToolListIssues_BadSegmentDegradesToEmpty asserts an unknown segment is data,
// not a transport error: the bridge returns an empty list (no rows match the join)
// and the tool surfaces an empty, error-free payload.
func TestToolListIssues_BadSegmentDegradesToEmpty(t *testing.T) {
	t.Parallel()
	// A healthy bridge that simply matches nothing for an unknown segment.
	mb := &mockBridge{issues: nil}
	h := listIssuesHandler(mb)

	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, ListIssuesInput{Segment: "no-such-segment"})
	if err != nil {
		t.Fatalf("unknown segment must be a clean empty result, not a Go error: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unknown segment set Error=%q, want empty (it is not a daemon-down condition)", out.Error)
	}
	if out.Issues == nil {
		t.Fatal("Issues nil; want a non-nil empty slice for [] JSON")
	}
	if len(out.Issues) != 0 {
		t.Fatalf("unknown segment returned %d issues, want 0", len(out.Issues))
	}
}

// TestToolGetSite_ExposesSegments asserts the get_site tool surfaces the site's
// configured segments so an agent can discover the filterable names.
func TestToolGetSite_ExposesSegments(t *testing.T) {
	t.Parallel()
	mb := &mockBridge{site: SiteDetail{
		ID:   5,
		URL:  "https://acme.test",
		Name: "Acme",
		Segments: []SegmentView{
			{Name: "content", Match: "^/blog/", MemberCount: 12},
			{Name: "product", Match: "^/product/", MemberCount: 4},
		},
	}}
	h := getSiteHandler(mb)
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetSiteInput{SiteID: 5})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if len(out.Detail.Segments) != 2 {
		t.Fatalf("Segments = %+v, want two", out.Detail.Segments)
	}
	got := out.Detail.Segments[0]
	if got.Name != "content" || got.Match != "^/blog/" || got.MemberCount != 12 {
		t.Fatalf("first segment = %+v, want {content ^/blog/ 12}", got)
	}
}

// TestControlBridgeSiteDecodesSegments asserts the production bridge decodes the
// daemon's segments list off the GET /v1/sites/{id}/detail JSON straight into the
// SiteDetail wire DTO (the seam: control.SegmentSummary == mcp.SegmentView).
func TestControlBridgeSiteDecodesSegments(t *testing.T) {
	t.Parallel()
	const body = `{"id":7,"url":"https://a.test","segments":[{"name":"content","match":"^/blog/","member_count":3}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sites/7/detail" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	b := NewControlBridge(control.NewClientWithBaseURL(srv.URL, "tok"))
	got, err := b.Site(context.Background(), 7)
	if err != nil {
		t.Fatalf("Site: %v", err)
	}
	if len(got.Segments) != 1 {
		t.Fatalf("Segments = %+v, want one decoded segment", got.Segments)
	}
	s := got.Segments[0]
	if s.Name != "content" || s.Match != "^/blog/" || s.MemberCount != 3 {
		t.Fatalf("decoded segment = %+v, want {content ^/blog/ 3}", s)
	}
}

// TestControlBridgeReportSendsSegment asserts the production bridge puts a non-empty
// ReportQuery.Segment onto the GET /v1/report request so the daemon can scope it.
func TestControlBridgeReportSendsSegment(t *testing.T) {
	t.Parallel()
	var gotSegment string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSegment = r.URL.Query().Get("segment")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"since":"0001-01-01T00:00:00Z","until":"2026-06-12T00:00:00Z","changes":{},"issues":{}}`))
	}))
	t.Cleanup(srv.Close)

	b := NewControlBridge(control.NewClientWithBaseURL(srv.URL, "tok"))
	if _, err := b.Report(context.Background(), ReportQuery{Segment: "content"}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if gotSegment != "content" {
		t.Fatalf("daemon saw segment %q, want content", gotSegment)
	}
}
