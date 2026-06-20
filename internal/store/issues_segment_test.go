package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// seedIssueWithSegment creates a site, a URL, a segment, an issue on that URL,
// and (when member) makes the URL a member of the segment. It returns the
// (site, url) ids so callers can assert site/segment scoping.
func seedIssueWithSegment(t *testing.T, st *store.DB, host, ruleID, segment string, member bool) (siteID, urlID int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	var err error
	siteID, err = st.AddSite(ctx, model.Site{
		BaseURL: "https://" + host, Name: host, Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite(%q): %v", host, err)
	}
	urlID, err = st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: "https://" + host + "/p", FirstSeen: now,
		NextCheckAt: now, Interval: 600,
	})
	if err != nil {
		t.Fatalf("UpsertURL(%q): %v", host, err)
	}
	ids, err := st.SyncSiteSegments(ctx, siteID, []model.Segment{
		{SiteID: siteID, Name: segment, MatchRule: "^/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments(%q): %v", host, err)
	}
	if member {
		if err := st.SetURLSegments(ctx, urlID, []int64{ids[segment]}); err != nil {
			t.Fatalf("SetURLSegments: %v", err)
		}
	}
	if _, err := st.UpsertIssue(ctx, model.Issue{
		URLID: urlID, RuleID: ruleID, Status: model.IssueOpen, Severity: model.SeverityWarning,
		ImpactPoints: 1, OpenedAt: now, LastSeenAt: now, Detail: "{}",
	}); err != nil {
		t.Fatalf("UpsertIssue: %v", err)
	}
	return siteID, urlID
}

// TestListIssuesFilterBySegment asserts the Segment filter on ListIssues returns
// only issues whose URL is a member of a segment with that name, and that the
// filtered result is a subset of the unfiltered result.
func TestListIssuesFilterBySegment(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// member URL in segment "content"; a non-member URL on the same site.
	siteID, _ := seedIssueWithSegment(t, st, "seg-content.test", "rule-member", "content", true)
	// non-member URL under the same site (no membership).
	now := time.Now().UTC()
	nonMemberURL, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: "https://seg-content.test/other", FirstSeen: now,
		NextCheckAt: now, Interval: 600,
	})
	if err != nil {
		t.Fatalf("UpsertURL(non-member): %v", err)
	}
	if _, err := st.UpsertIssue(ctx, model.Issue{
		URLID: nonMemberURL, RuleID: "rule-nonmember", Status: model.IssueOpen,
		Severity: model.SeverityWarning, ImpactPoints: 1, OpenedAt: now, LastSeenAt: now, Detail: "{}",
	}); err != nil {
		t.Fatalf("UpsertIssue(non-member): %v", err)
	}

	all, err := st.ListIssues(ctx, store.IssueFilter{})
	if err != nil {
		t.Fatalf("ListIssues(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered = %d issues, want 2", len(all))
	}

	seg := "content"
	got, err := st.ListIssues(ctx, store.IssueFilter{Segment: &seg})
	if err != nil {
		t.Fatalf("ListIssues(segment=content): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("segment=content = %d issues, want 1 (only the member)", len(got))
	}
	if got[0].RuleID != "rule-member" {
		t.Fatalf("segment-filtered issue = %q, want rule-member", got[0].RuleID)
	}
	if len(got) > len(all) {
		t.Fatalf("filtered (%d) > unfiltered (%d)", len(got), len(all))
	}

	// Unknown segment name → empty (no transport error).
	unknown := "nope"
	none, err := st.ListIssues(ctx, store.IssueFilter{Segment: &unknown})
	if err != nil {
		t.Fatalf("ListIssues(segment=nope): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("unknown segment = %d issues, want 0", len(none))
	}
}

// TestListIssuesSegmentScopedPerSite asserts that a name shared by two sites'
// segments matches members in BOTH sites for an all-sites query (names are
// scoped per-site; an all-sites query filtered by name matches that name in any
// site), and that combining Segment with SiteID narrows to the one site.
func TestListIssuesSegmentScopedPerSite(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	siteA, _ := seedIssueWithSegment(t, st, "a.seg.test", "rule-a", "content", true)
	siteB, _ := seedIssueWithSegment(t, st, "b.seg.test", "rule-b", "content", true)

	seg := "content"
	all, err := st.ListIssues(ctx, store.IssueFilter{Segment: &seg})
	if err != nil {
		t.Fatalf("ListIssues(segment, all sites): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all-sites segment=content = %d, want 2 (one per site)", len(all))
	}

	gotA, err := st.ListIssues(ctx, store.IssueFilter{Segment: &seg, SiteID: &siteA})
	if err != nil {
		t.Fatalf("ListIssues(segment+siteA): %v", err)
	}
	if len(gotA) != 1 || gotA[0].RuleID != "rule-a" {
		t.Fatalf("segment+siteA = %+v, want only rule-a", gotA)
	}
	_ = siteB
}
