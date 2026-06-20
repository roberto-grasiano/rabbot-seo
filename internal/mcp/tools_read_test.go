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

func TestToolGetStatus(t *testing.T) {
	t.Parallel()

	want := control.StatusResponse{Version: "9.9.9", SiteCount: 3, URLCount: 12, Paused: true}
	h := getStatusHandler(&mockBridge{status: want})
	res, out, err := h(context.Background(), &mcp.CallToolRequest{}, struct{}{})
	if err != nil {
		t.Fatalf("handler Go error: %v", err)
	}
	if out.Version != "9.9.9" || out.SiteCount != 3 || !out.Paused || out.URLCount != 12 {
		t.Fatalf("out = %+v, want %+v", out, want)
	}
	// res is nil: the SDK populates Content from the structured Out.
	if res != nil {
		t.Fatalf("res = %+v, want nil (let SDK build Content from Out)", res)
	}
}

// Criterion 11 (agent-verification read surface): get_status inherits the new
// MetricsAddr field via the embedded StatusResponse, with no per-field plumbing —
// the read surface the Claude-path agent uses to confirm /metrics is live.
func TestToolGetStatus_MetricsAddr(t *testing.T) {
	t.Parallel()

	want := control.StatusResponse{Version: "1", MetricsAddr: "127.0.0.1:9464"}
	h := getStatusHandler(&mockBridge{status: want})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, struct{}{})
	if err != nil {
		t.Fatalf("handler Go error: %v", err)
	}
	if out.MetricsAddr != "127.0.0.1:9464" {
		t.Fatalf("out.MetricsAddr = %q, want 127.0.0.1:9464 (inherited via StatusResponse)", out.MetricsAddr)
	}
}

func TestToolGetStatus_DaemonDown(t *testing.T) {
	t.Parallel()

	// Daemon down must read as DATA: nil Go error, an Out whose Error field carries
	// the friendly message; zeroed counts.
	h := getStatusHandler(&mockBridge{statusErr: control.ErrDaemonNotRunning})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, struct{}{})
	if err != nil {
		t.Fatalf("daemon down must not be a Go error, got: %v", err)
	}
	if out.Error != control.ErrDaemonNotRunning.Error() {
		t.Fatalf("out.Error = %q, want %q", out.Error, control.ErrDaemonNotRunning.Error())
	}
	if out.Version != "" || out.SiteCount != 0 {
		t.Fatalf("out = %+v, want zeroed status on down daemon", out)
	}
}

func TestToolListSites(t *testing.T) {
	t.Parallel()

	want := []SiteView{
		{ID: 1, URL: "https://a.test", Name: "A", Enabled: true, VerificationState: "verified"},
		{ID: 2, URL: "https://b.test", Name: "B", Enabled: false, VerificationState: "throttled"},
	}
	h := listSitesHandler(&mockBridge{sites: want})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, struct{}{})
	if err != nil {
		t.Fatalf("handler Go error: %v", err)
	}
	if !reflect.DeepEqual(out.Sites, want) {
		t.Fatalf("out.Sites = %+v, want %+v", out.Sites, want)
	}
	if out.Error != "" {
		t.Fatalf("out.Error = %q, want empty", out.Error)
	}
}

func TestToolListSites_NilNormalizesToEmpty(t *testing.T) {
	t.Parallel()

	h := listSitesHandler(&mockBridge{sites: nil})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, struct{}{})
	if err != nil {
		t.Fatalf("handler Go error: %v", err)
	}
	if out.Sites == nil {
		t.Fatalf("out.Sites = nil, want non-nil empty slice (serializes as [])")
	}
	if len(out.Sites) != 0 {
		t.Fatalf("out.Sites len = %d, want 0", len(out.Sites))
	}
	// Confirm it marshals to a JSON array, not null.
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"sites":[]`) {
		t.Fatalf("marshalled = %s, want \"sites\":[]", b)
	}
}

func TestToolListSites_DaemonDown(t *testing.T) {
	t.Parallel()

	h := listSitesHandler(&mockBridge{sitesErr: control.ErrDaemonNotRunning})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, struct{}{})
	if err != nil {
		t.Fatalf("daemon down must not be a Go error, got: %v", err)
	}
	if out.Error != control.ErrDaemonNotRunning.Error() {
		t.Fatalf("out.Error = %q, want %q", out.Error, control.ErrDaemonNotRunning.Error())
	}
	if out.Sites == nil {
		t.Fatalf("out.Sites = nil on error, want non-nil empty slice")
	}
}

func TestToolGetSite(t *testing.T) {
	t.Parallel()

	want := SiteDetail{ID: 5, URL: "https://acme.test", Name: "Acme", Enabled: true, VerificationState: "verified", OpenIssueCount: 2, HasSnapshot: true, Title: "Home"}
	h := getSiteHandler(&mockBridge{site: want})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetSiteInput{SiteID: 5})
	if err != nil {
		t.Fatalf("handler Go error: %v", err)
	}
	if !reflect.DeepEqual(out.Detail, want) {
		t.Fatalf("out.Detail = %+v, want %+v", out.Detail, want)
	}
	if out.Error != "" {
		t.Fatalf("out.Error = %q, want empty", out.Error)
	}
}

// TestSiteDetailIndexableFalseSerializes pins that a genuine indexable=false is
// present in the get_site JSON (no omitempty), so a consumer can distinguish
// "not indexable" from "field absent" (no snapshot yet).
func TestSiteDetailIndexableFalseSerializes(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(SiteDetail{ID: 5, HasSnapshot: true, Indexable: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"indexable":false`) {
		t.Fatalf("indexable=false was dropped from JSON: %s", raw)
	}

	raw, err = json.Marshal(SiteDetail{ID: 5, HasSnapshot: true, Indexable: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"indexable":true`) {
		t.Fatalf("indexable=true missing from JSON: %s", raw)
	}
}

func TestToolGetSite_NotFoundIsData(t *testing.T) {
	t.Parallel()

	// An unknown id is errors-as-data: the bridge returns NotFound=true with a nil
	// Go error, and the tool surfaces it as a normal object (not a tool error).
	h := getSiteHandler(&mockBridge{site: SiteDetail{NotFound: true}})
	res, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetSiteInput{SiteID: 999})
	if err != nil {
		t.Fatalf("not-found must not be a Go error, got: %v", err)
	}
	if res != nil {
		t.Fatalf("res = %+v, want nil", res)
	}
	if !out.Detail.NotFound {
		t.Fatalf("out.Detail.NotFound = false, want true")
	}
	if out.Error != "" {
		t.Fatalf("out.Error = %q, want empty (not-found is data, not an error)", out.Error)
	}
}

func TestToolGetSite_DaemonDown(t *testing.T) {
	t.Parallel()

	h := getSiteHandler(&mockBridge{siteErr: control.ErrDaemonNotRunning})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetSiteInput{SiteID: 5})
	if err != nil {
		t.Fatalf("daemon down must not be a Go error, got: %v", err)
	}
	if out.Error != control.ErrDaemonNotRunning.Error() {
		t.Fatalf("out.Error = %q, want %q", out.Error, control.ErrDaemonNotRunning.Error())
	}
}

func TestToolListIssues(t *testing.T) {
	t.Parallel()

	want := []IssueView{
		{ID: 11, URLID: 3, RuleID: "title-missing", Status: "open", Severity: "critical", ImpactPoints: 9, Detail: "no <title>"},
		{ID: 12, URLID: 3, RuleID: "meta-desc", Status: "open", Severity: "warning", ImpactPoints: 3},
	}
	mb := &mockBridge{issues: want}
	h := listIssuesHandler(mb)
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, ListIssuesInput{Severity: "critical"})
	if err != nil {
		t.Fatalf("handler Go error: %v", err)
	}
	if !reflect.DeepEqual(out.Issues, want) {
		t.Fatalf("out.Issues = %+v, want %+v", out.Issues, want)
	}
}

func TestToolListIssues_QueryMapping(t *testing.T) {
	t.Parallel()

	// site_id=0 means "no site filter" -> IssueQuery.SiteID must be nil; a non-zero
	// id is passed as a pointer. severity/status pass through verbatim.
	mb := &mockBridge{}
	h := listIssuesHandler(mb)

	if _, _, err := h(context.Background(), &mcp.CallToolRequest{}, ListIssuesInput{SiteID: 0, Severity: "warning", Status: "open"}); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if mb.lastIssueQuery.SiteID != nil {
		t.Fatalf("SiteID = %v, want nil for site_id=0", *mb.lastIssueQuery.SiteID)
	}
	if mb.lastIssueQuery.Severity != "warning" || mb.lastIssueQuery.Status != "open" {
		t.Fatalf("query = %+v, want severity=warning status=open", mb.lastIssueQuery)
	}

	if _, _, err := h(context.Background(), &mcp.CallToolRequest{}, ListIssuesInput{SiteID: 7}); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if mb.lastIssueQuery.SiteID == nil || *mb.lastIssueQuery.SiteID != 7 {
		t.Fatalf("SiteID = %v, want pointer to 7", mb.lastIssueQuery.SiteID)
	}
}

func TestToolListIssues_NilNormalizes(t *testing.T) {
	t.Parallel()

	h := listIssuesHandler(&mockBridge{issues: nil})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, ListIssuesInput{})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if out.Issues == nil {
		t.Fatalf("out.Issues = nil, want non-nil empty slice")
	}
}

func TestToolListIssues_DaemonDown(t *testing.T) {
	t.Parallel()

	h := listIssuesHandler(&mockBridge{issuesErr: control.ErrDaemonNotRunning})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, ListIssuesInput{})
	if err != nil {
		t.Fatalf("daemon down must not be a Go error, got: %v", err)
	}
	if out.Error != control.ErrDaemonNotRunning.Error() {
		t.Fatalf("out.Error = %q, want %q", out.Error, control.ErrDaemonNotRunning.Error())
	}
	if out.Issues == nil {
		t.Fatalf("out.Issues = nil on error, want non-nil empty slice")
	}
}

func TestToolGetHistory(t *testing.T) {
	t.Parallel()

	want := HistoryView{
		URL: "https://acme.test/p",
		Changes: []ChangeView{
			{Field: "title", OldValue: "Old", NewValue: "New", ChangeClass: "substantive", DetectedAt: "2026-06-01T10:00:00Z"},
		},
	}
	mb := &mockBridge{history: want}
	h := getHistoryHandler(mb)
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetHistoryInput{URL: "https://acme.test/p"})
	if err != nil {
		t.Fatalf("handler Go error: %v", err)
	}
	if out.History.URL != want.URL || len(out.History.Changes) != 1 || out.History.Changes[0].Field != "title" {
		t.Fatalf("out.History = %+v, want %+v", out.History, want)
	}
	// Empty since => zero time passed to the bridge (all history).
	if !mb.lastHistorySince.IsZero() {
		t.Fatalf("since = %v, want zero for empty input", mb.lastHistorySince)
	}
	if mb.lastHistoryURL != "https://acme.test/p" {
		t.Fatalf("url = %q, want the input url", mb.lastHistoryURL)
	}
}

func TestToolGetHistory_SinceParsed(t *testing.T) {
	t.Parallel()

	mb := &mockBridge{}
	h := getHistoryHandler(mb)
	_, _, err := h(context.Background(), &mcp.CallToolRequest{}, GetHistoryInput{URL: "https://x.test", Since: "2026-06-05T00:00:00Z"})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	want := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	if !mb.lastHistorySince.Equal(want) {
		t.Fatalf("since = %v, want %v", mb.lastHistorySince, want)
	}
}

func TestToolGetHistory_BadSinceIsToolError(t *testing.T) {
	t.Parallel()

	h := getHistoryHandler(&mockBridge{})
	res, _, err := h(context.Background(), &mcp.CallToolRequest{}, GetHistoryInput{URL: "https://x.test", Since: "yesterday"})
	// Malformed caller input is a tool error (model can fix it), NOT data.
	if err == nil {
		t.Fatalf("malformed since must be a tool error, got nil")
	}
	if res != nil {
		t.Fatalf("res = %+v, want nil (error carried via returned Go error)", res)
	}
}

func TestToolGetHistory_NotFoundIsData(t *testing.T) {
	t.Parallel()

	h := getHistoryHandler(&mockBridge{history: HistoryView{URL: "https://x.test", NotFound: true}})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetHistoryInput{URL: "https://x.test"})
	if err != nil {
		t.Fatalf("not-found must not be a Go error, got: %v", err)
	}
	if !out.History.NotFound {
		t.Fatalf("out.History.NotFound = false, want true")
	}
	if out.Error != "" {
		t.Fatalf("out.Error = %q, want empty (not-found is data)", out.Error)
	}
}

func TestToolGetHistory_DaemonDown(t *testing.T) {
	t.Parallel()

	h := getHistoryHandler(&mockBridge{historyErr: control.ErrDaemonNotRunning})
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, GetHistoryInput{URL: "https://x.test"})
	if err != nil {
		t.Fatalf("daemon down must not be a Go error, got: %v", err)
	}
	if out.Error != control.ErrDaemonNotRunning.Error() {
		t.Fatalf("out.Error = %q, want %q", out.Error, control.ErrDaemonNotRunning.Error())
	}
}

func TestSummarizeChangesHandler_Default(t *testing.T) {
	t.Parallel()
	m := &mockBridge{report: ReportView{Changes: ReportChangeSummary{Total: 4, Substantive: 3, Cosmetic: 1}}}
	h := summarizeChangesHandler(m)
	_, out, err := h(context.Background(), nil, SummarizeChangesInput{})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if out.Report.Changes.Total != 4 || out.Error != "" {
		t.Fatalf("out = %+v", out)
	}
	// default window = 168h (the tool's documented contract): assert the resolved
	// Since is ~7d before now, not merely non-zero — a bug setting the default to
	// 24h (or any other value) must fail here. ±1h tolerance covers execution time.
	if d := time.Since(m.lastReportQuery.Since); d < 167*time.Hour || d > 169*time.Hour {
		t.Fatalf("default window = %v, want ~168h (7 days)", d)
	}
	if m.lastReportQuery.TopN != 10 {
		t.Fatalf("default TopN = %d, want 10", m.lastReportQuery.TopN)
	}
}

func TestSummarizeChangesHandler_AdHocAndSite(t *testing.T) {
	t.Parallel()
	m := &mockBridge{}
	h := summarizeChangesHandler(m)
	if _, _, err := h(context.Background(), nil, SummarizeChangesInput{Since: "24h", SiteID: 7, Limit: 3}); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if m.lastReportQuery.SiteID == nil || *m.lastReportQuery.SiteID != 7 || m.lastReportQuery.TopN != 3 {
		t.Fatalf("query = %+v", m.lastReportQuery)
	}
}

func TestSummarizeChangesHandler_BadSince(t *testing.T) {
	t.Parallel()
	h := summarizeChangesHandler(&mockBridge{})
	if _, _, err := h(context.Background(), nil, SummarizeChangesInput{Since: "banana"}); err == nil {
		t.Fatalf("expected tool error for malformed since")
	}
}

func TestSummarizeChangesHandler_DaemonDown(t *testing.T) {
	t.Parallel()
	m := &mockBridge{reportErr: control.ErrDaemonNotRunning}
	h := summarizeChangesHandler(m)
	_, out, err := h(context.Background(), nil, SummarizeChangesInput{})
	if err != nil {
		t.Fatalf("daemon-down must be data, not error: %v", err)
	}
	if out.Error == "" {
		t.Fatalf("expected errors-as-data Error field")
	}
}
