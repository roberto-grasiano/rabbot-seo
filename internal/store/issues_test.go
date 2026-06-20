package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// seedURL creates a site + URL so foreign-key constraints (PRAGMA
// foreign_keys = ON, set by the M0 connection hook) resolve, and returns the
// URL id to use as a parent for issues/changes. The exact id value is opaque;
// callers assert on row contents, not on the literal id.
func seedURL(t *testing.T, st *store.DB, host string) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	siteID, err := st.AddSite(ctx, model.Site{
		BaseURL: "https://" + host, Name: host, Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite(%q): %v", host, err)
	}
	urlID, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: "https://" + host + "/p", FirstSeen: now,
		NextCheckAt: now, Interval: 600,
	})
	if err != nil {
		t.Fatalf("UpsertURL(%q): %v", host, err)
	}
	return urlID
}

// seedIssueFixture inserts a site, a URL, and one issue at the given severity,
// returning the url id so the test can assert filtered reads.
func seedIssueFixture(t *testing.T, st *store.DB, sev model.Severity, ruleID string) int64 {
	t.Helper()
	urlID := seedURL(t, st, "sev-"+ruleID+".test")
	now := time.Now().UTC()
	if _, err := st.UpsertIssue(context.Background(), model.Issue{
		URLID: urlID, RuleID: ruleID, Status: model.IssueOpen, Severity: sev,
		ImpactPoints: 1, OpenedAt: now, LastSeenAt: now, Detail: "{}",
	}); err != nil {
		t.Fatalf("UpsertIssue: %v", err)
	}
	return urlID
}

func TestListIssuesFilterBySeverity(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	seedIssueFixture(t, st, model.SeverityCritical, "rule-crit")
	seedIssueFixture(t, st, model.SeverityWarning, "rule-warn")

	crit := model.SeverityCritical
	got, err := st.ListIssues(ctx, store.IssueFilter{Severity: &crit})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("severity=critical returned %d issues, want 1", len(got))
	}
	if got[0].Severity != model.SeverityCritical || got[0].RuleID != "rule-crit" {
		t.Fatalf("got %+v, want the critical issue", got[0])
	}

	// No severity filter returns both.
	all, err := st.ListIssues(ctx, store.IssueFilter{})
	if err != nil {
		t.Fatalf("ListIssues(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("no-filter returned %d issues, want 2", len(all))
	}
}

func TestUpsertAndListIssuesOpenOnly(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().UTC()
	urlID := seedURL(t, st, "issues-open.com")

	if _, err := st.UpsertIssue(ctx, model.Issue{
		URLID: urlID, RuleID: "indexability_flip", Status: model.IssueOpen,
		Severity: model.SeverityCritical, ImpactPoints: 1000, OpenedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("UpsertIssue: %v", err)
	}
	open, err := st.ListIssues(ctx, store.IssueFilter{OpenOnly: true})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(open) != 1 || open[0].RuleID != "indexability_flip" {
		t.Fatalf("ListIssues open = %+v", open)
	}
	if err := st.CloseIssue(ctx, urlID, "indexability_flip", now.Add(time.Minute)); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	open, _ = st.ListIssues(ctx, store.IssueFilter{OpenOnly: true})
	if len(open) != 0 {
		t.Errorf("after close, open issues = %d, want 0", len(open))
	}
}

func TestIgnoreIssue(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().UTC()
	urlID := seedURL(t, st, "issues-ignore.com")

	id, err := st.UpsertIssue(ctx, model.Issue{
		URLID: urlID, RuleID: "thin_content", Status: model.IssueOpen,
		Severity: model.SeverityWarning, ImpactPoints: 100, OpenedAt: now, LastSeenAt: now,
	})
	if err != nil {
		t.Fatalf("UpsertIssue: %v", err)
	}
	if err := st.IgnoreIssue(ctx, id); err != nil {
		t.Fatalf("IgnoreIssue: %v", err)
	}
	// An ignored issue is neither open nor closed; OpenOnly must drop it.
	open, _ := st.ListIssues(ctx, store.IssueFilter{OpenOnly: true})
	if len(open) != 0 {
		t.Errorf("ignored issue still listed open: %+v", open)
	}
	ignored := model.IssueIgnored
	got, err := st.ListIssues(ctx, store.IssueFilter{Status: &ignored})
	if err != nil {
		t.Fatalf("ListIssues(ignored): %v", err)
	}
	if len(got) != 1 || got[0].Status != model.IssueIgnored {
		t.Fatalf("ListIssues(ignored) = %+v, want 1 ignored", got)
	}
}

// TestUpsertIssueReopenOpenedAt covers the closed->open reopen contract: a
// reopen must take the fresh opened_at and clear closed_at (the row must not
// look like it was never closed), while a plain open->open refresh (no
// intervening close) keeps the ORIGINAL opened_at.
func TestUpsertIssueReopenOpenedAt(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	urlID := seedURL(t, st, "issues-reopen.com")

	t0 := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Minute)
	t2 := t0.Add(2 * time.Hour)

	mk := func(opened, seen time.Time) model.Issue {
		return model.Issue{
			URLID: urlID, RuleID: "indexability_flip", Status: model.IssueOpen,
			Severity: model.SeverityCritical, ImpactPoints: 1000, OpenedAt: opened, LastSeenAt: seen,
		}
	}

	type step struct {
		name   string
		reopen bool // true => close before this upsert
		opened time.Time
		seen   time.Time
		// expectations after this upsert
		wantOpened time.Time
		wantClosed bool // true => closed_at must be non-NULL
	}
	steps := []step{
		{
			name: "initial open", opened: t0, seen: t0,
			wantOpened: t0, wantClosed: false,
		},
		{
			// open->open refresh (no intervening close) keeps original opened_at.
			name: "open->open refresh keeps opened_at", opened: t1, seen: t1,
			wantOpened: t0, wantClosed: false,
		},
		{
			// closed->open reopen must take the fresh opened_at and clear closed_at.
			name: "closed->open reopen takes fresh opened_at", reopen: true, opened: t2, seen: t2,
			wantOpened: t2, wantClosed: false,
		},
	}

	for _, s := range steps {
		if s.reopen {
			if err := st.CloseIssue(ctx, urlID, "indexability_flip", s.opened.Add(-time.Minute)); err != nil {
				t.Fatalf("%s: CloseIssue: %v", s.name, err)
			}
		}
		if _, err := st.UpsertIssue(ctx, mk(s.opened, s.seen)); err != nil {
			t.Fatalf("%s: UpsertIssue: %v", s.name, err)
		}

		var (
			gotOpened time.Time
			closedAt  sql.NullTime
			status    string
		)
		if err := st.Read().QueryRowContext(ctx,
			`SELECT opened_at, closed_at, status FROM issues WHERE url_id = ? AND rule_id = ?`,
			urlID, "indexability_flip").Scan(&gotOpened, &closedAt, &status); err != nil {
			t.Fatalf("%s: read back: %v", s.name, err)
		}
		if !gotOpened.Equal(s.wantOpened) {
			t.Errorf("%s: opened_at = %v, want %v", s.name, gotOpened, s.wantOpened)
		}
		if closedAt.Valid != s.wantClosed {
			t.Errorf("%s: closed_at valid = %v, want %v (closed_at=%v)", s.name, closedAt.Valid, s.wantClosed, closedAt)
		}
		if status != string(model.IssueOpen) {
			t.Errorf("%s: status = %q, want open", s.name, status)
		}
	}
}

// TestIgnoreIssueRowsAffected pins IgnoreIssue's contract: a real ignore of an
// existing id succeeds and flips status; a missing id returns ErrNotFound. The
// fix checks the RowsAffected error rather than swallowing it with `_`, so a
// genuine DB failure can no longer be masked as ErrNotFound.
func TestIgnoreIssueRowsAffected(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().UTC()
	urlID := seedURL(t, st, "issues-ignore-rows.com")

	id, err := st.UpsertIssue(ctx, model.Issue{
		URLID: urlID, RuleID: "missing_canonical", Status: model.IssueOpen,
		Severity: model.SeverityWarning, ImpactPoints: 100, OpenedAt: now, LastSeenAt: now,
	})
	if err != nil {
		t.Fatalf("UpsertIssue: %v", err)
	}

	tests := []struct {
		name    string
		id      int64
		wantErr error
	}{
		{name: "existing id ignored", id: id, wantErr: nil},
		{name: "missing id is ErrNotFound", id: id + 9999, wantErr: store.ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := st.IgnoreIssue(ctx, tc.id)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("IgnoreIssue(%d) err = %v, want %v", tc.id, err, tc.wantErr)
			}
		})
	}

	ignored := model.IssueIgnored
	got, err := st.ListIssues(ctx, store.IssueFilter{Status: &ignored})
	if err != nil {
		t.Fatalf("ListIssues(ignored): %v", err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("ListIssues(ignored) = %+v, want 1 ignored id=%d", got, id)
	}
}

func TestUpsertIssueDedupKeyURLRule(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().UTC()
	urlID := seedURL(t, st, "issues-dedup.com")

	first := model.Issue{
		URLID: urlID, RuleID: "missing_title", Status: model.IssueOpen,
		Severity: model.SeverityWarning, ImpactPoints: 50, OpenedAt: now, LastSeenAt: now,
	}
	if _, err := st.UpsertIssue(ctx, first); err != nil {
		t.Fatalf("UpsertIssue#1: %v", err)
	}
	// Same (url_id, rule_id) re-upsert must update in place, not duplicate.
	first.Severity = model.SeverityCritical
	first.ImpactPoints = 1000
	first.LastSeenAt = now.Add(time.Hour)
	if _, err := st.UpsertIssue(ctx, first); err != nil {
		t.Fatalf("UpsertIssue#2: %v", err)
	}
	open, _ := st.ListIssues(ctx, store.IssueFilter{OpenOnly: true})
	if len(open) != 1 {
		t.Fatalf("dedup (url_id,rule_id) broke: got %d issues, want 1", len(open))
	}
	if open[0].Severity != model.SeverityCritical || open[0].ImpactPoints != 1000 {
		t.Errorf("re-upsert did not update severity/impact: %+v", open[0])
	}
}
