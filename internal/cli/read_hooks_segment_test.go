package cli

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// seedSegmentedStore extends openSeededStore with a `content` segment and a
// second issue on a /blog member URL, so a segment filter has both a member
// (the blog issue) and a non-member (the seeded homepage issue) to discriminate.
// It returns the db, the site id, and the content segment id.
func seedSegmentedStore(t *testing.T) (*store.DB, int64, int64) {
	t.Helper()
	ctx := context.Background()
	db, siteID := openSeededStore(t)

	// Define a `content` segment matching /blog/ and sync it.
	ids, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{
		{SiteID: siteID, Name: "content", MatchRule: "^/blog/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}
	contentID := ids["content"]
	if contentID == 0 {
		t.Fatal("content segment id not returned")
	}

	// A /blog member URL with its own open issue.
	blogID, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://a.test/blog/post", NextCheckAt: time.Now()})
	if err != nil {
		t.Fatalf("UpsertURL blog: %v", err)
	}
	if err := db.SetURLSegments(ctx, blogID, []int64{contentID}); err != nil {
		t.Fatalf("SetURLSegments: %v", err)
	}
	now := time.Now().UTC()
	if _, err := db.UpsertIssue(ctx, model.Issue{
		URLID: blogID, RuleID: "missing-title", Status: model.IssueOpen,
		Severity: model.SeverityCritical, ImpactPoints: 9, OpenedAt: now, LastSeenAt: now, Detail: "{}",
	}); err != nil {
		t.Fatalf("UpsertIssue blog: %v", err)
	}
	return db, siteID, contentID
}

// TestIssuesHookSegmentFilter asserts the daemon hook scopes by segment name:
// `content` returns only the /blog member issue, never the homepage issue.
func TestIssuesHookSegmentFilter(t *testing.T) {
	db, _, _ := seedSegmentedStore(t)
	hook := issuesHook(db)

	got, err := hook(context.Background(), control.IssueQuery{Segment: "content"})
	if err != nil {
		t.Fatalf("issuesHook(segment=content): %v", err)
	}
	if len(got) != 1 || got[0].RuleID != "missing-title" {
		t.Fatalf("segment=content got %+v, want only the /blog issue", got)
	}

	// An unknown segment name is NOT an error: it degrades to empty data.
	none, err := hook(context.Background(), control.IssueQuery{Segment: "no-such-segment"})
	if err != nil {
		t.Fatalf("issuesHook(unknown segment) must be data, not error: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("unknown segment got %d issues, want 0", len(none))
	}
}

// TestReportHookSegmentFilter asserts the report hook passes the segment through
// to BuildReport: a `content`-scoped report counts only the /blog member's issue.
func TestReportHookSegmentFilter(t *testing.T) {
	db, _, _ := seedSegmentedStore(t)
	hook := reportHook(db)
	seg := "content"

	scoped, err := hook(context.Background(), time.Time{}, nil, 0, &seg)
	if err != nil {
		t.Fatalf("reportHook(segment): %v", err)
	}
	all, err := hook(context.Background(), time.Time{}, nil, 0, nil)
	if err != nil {
		t.Fatalf("reportHook(all): %v", err)
	}
	// The /blog issue is critical; the homepage issue is warning. Scoped to content,
	// only the critical one counts — and the scoped total is <= the unfiltered total.
	if scoped.Issues.OpenTotal != 1 || scoped.Issues.OpenCritical != 1 {
		t.Fatalf("scoped issues = %+v, want exactly the 1 critical /blog issue", scoped.Issues)
	}
	if scoped.Issues.OpenTotal > all.Issues.OpenTotal {
		t.Fatalf("scoped total %d must be <= unfiltered total %d", scoped.Issues.OpenTotal, all.Issues.OpenTotal)
	}
}

// TestSiteDetailHookExposesSegments asserts the site-detail hook lists the
// configured segments (name, pattern, member count) for agent discovery.
func TestSiteDetailHookExposesSegments(t *testing.T) {
	db, siteID, _ := seedSegmentedStore(t)
	hook := siteDetailHook(db, noCapResolver)
	got, found, err := hook(context.Background(), siteID)
	if err != nil || !found {
		t.Fatalf("siteDetailHook: found=%v err=%v", found, err)
	}
	if len(got.Segments) != 1 {
		t.Fatalf("Segments = %+v, want exactly one", got.Segments)
	}
	s := got.Segments[0]
	if s.Name != "content" || s.Match != "^/blog/" || s.MemberCount != 1 {
		t.Fatalf("segment summary = %+v, want {content ^/blog/ 1}", s)
	}
}

// TestSiteDetailHookSegmentsNonNil asserts a site with no configured segments
// reports an empty (non-nil) slice, so the JSON is [] not null.
func TestSiteDetailHookSegmentsNonNil(t *testing.T) {
	db, siteID := openSeededStore(t) // no segments synced
	hook := siteDetailHook(db, noCapResolver)
	got, _, err := hook(context.Background(), siteID)
	if err != nil {
		t.Fatalf("siteDetailHook: %v", err)
	}
	if got.Segments == nil {
		t.Fatal("Segments is nil; want a non-nil empty slice for [] JSON")
	}
	if len(got.Segments) != 0 {
		t.Fatalf("Segments = %+v, want empty", got.Segments)
	}
}

// TestControlSegmentFilterEquivalentToStore is the acceptance-criterion-7 proof:
// GET /v1/issues?segment= over the live control server (httptest) returns exactly
// the same issue set as the direct store filter store.IssueFilter{Segment:...}.
func TestControlSegmentFilterEquivalentToStore(t *testing.T) {
	ctx := context.Background()
	db, _, _ := seedSegmentedStore(t)

	// Stand up a real control server backed by the production read hooks.
	srv := control.NewServer(control.ServerOptions{
		Token:   "tok",
		Version: "0.1.0",
		Hooks: control.Hooks{
			Issues: issuesHook(db),
			Report: reportHook(db),
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client := control.NewClientWithBaseURL(ts.URL, "tok")

	// Control path: GET /v1/issues?segment=content.
	overControl, err := client.Issues(ctx, nil, "", "", "content")
	if err != nil {
		t.Fatalf("client.Issues(segment): %v", err)
	}

	// Store path: the direct filter the CLI `issues --segment` takes.
	seg := "content"
	storeIssues, err := db.ListIssues(ctx, store.IssueFilter{Segment: &seg})
	if err != nil {
		t.Fatalf("db.ListIssues(segment): %v", err)
	}

	// Same membership, same order: project both to the rule-id sequence.
	if len(overControl) != len(storeIssues) {
		t.Fatalf("control returned %d issues, store returned %d", len(overControl), len(storeIssues))
	}
	gotRules := make([]string, len(overControl))
	for i, v := range overControl {
		gotRules[i] = v.RuleID
	}
	wantRules := make([]string, len(storeIssues))
	for i, v := range storeIssues {
		wantRules[i] = v.RuleID
	}
	if !reflect.DeepEqual(gotRules, wantRules) {
		t.Fatalf("control filter %v != store filter %v", gotRules, wantRules)
	}
	if len(gotRules) != 1 || gotRules[0] != "missing-title" {
		t.Fatalf("segment=content rule set = %v, want [missing-title]", gotRules)
	}

	// A bad segment value degrades to empty data over the wire — never an error.
	bad, err := client.Issues(ctx, nil, "", "", "no-such-segment")
	if err != nil {
		t.Fatalf("bad segment over control must be data, not a transport error: %v", err)
	}
	if len(bad) != 0 {
		t.Fatalf("bad segment returned %d issues, want 0", len(bad))
	}

	// Report equivalence: ?segment= over the wire matches the store-scoped digest.
	overReport, err := client.Report(ctx, time.Time{}, nil, 0, &seg)
	if err != nil {
		t.Fatalf("client.Report(segment): %v", err)
	}
	storeReport, err := db.BuildReport(ctx, store.ReportParams{Segment: &seg})
	if err != nil {
		t.Fatalf("db.BuildReport(segment): %v", err)
	}
	if overReport.Issues.OpenTotal != storeReport.Issues.OpenTotal ||
		overReport.Issues.OpenCritical != storeReport.Issues.OpenCritical {
		t.Fatalf("report issue rollup over control %+v != store %+v", overReport.Issues, storeReport.Issues)
	}
}
