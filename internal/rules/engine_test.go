package rules

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

type fakeIssueStore struct {
	open    map[string]model.Issue // key "url_id|rule_id"
	upserts []model.Issue
	closes  []string
}

func newFakeIssueStore() *fakeIssueStore {
	return &fakeIssueStore{open: map[string]model.Issue{}}
}

// key matches the store's UNIQUE(url_id, rule_id) identity: issues are scoped
// per URL, so the same rule failing on two different URLs is two distinct issues.
func key(urlID int64, ruleID string) string {
	return strconv.FormatInt(urlID, 10) + "|" + ruleID
}

func (f *fakeIssueStore) ListIssues(ctx context.Context, filter store.IssueFilter) ([]model.Issue, error) {
	var out []model.Issue
	for _, iss := range f.open {
		if filter.URLID != nil && iss.URLID != *filter.URLID {
			continue
		}
		out = append(out, iss)
	}
	return out, nil
}
func (f *fakeIssueStore) UpsertIssue(ctx context.Context, iss model.Issue) (int64, error) {
	f.upserts = append(f.upserts, iss)
	f.open[key(iss.URLID, iss.RuleID)] = iss
	return 1, nil
}
func (f *fakeIssueStore) CloseIssue(ctx context.Context, urlID int64, ruleID string, at time.Time) error {
	f.closes = append(f.closes, ruleID)
	delete(f.open, key(urlID, ruleID))
	return nil
}

func TestEngineOpensIssueOnFailure(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	fs := newFakeIssueStore()
	eng := NewEngine(DefaultRuleSet(), fs, func() time.Time { return now })

	ctx := EvalContext{
		URLID:      5,
		Importance: 1.0,
		New:        model.Snapshot{Indexable: false, Title: "T", Canonical: "https://x/a", MetaRobots: "noindex", HTTPStatus: 200, Headings: `{"h1":["x"]}`, MetaDescription: "d"},
		Old:        model.Snapshot{ID: 1, Indexable: true, Title: "T", Canonical: "https://x/a", HTTPStatus: 200},
	}
	if err := eng.Apply(context.Background(), ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var idx *model.Issue
	for i := range fs.upserts {
		if fs.upserts[i].RuleID == "indexability_flip" {
			idx = &fs.upserts[i]
		}
	}
	if idx == nil {
		t.Fatalf("expected indexability_flip issue opened, upserts=%+v", fs.upserts)
	}
	if idx.Status != model.IssueOpen || idx.Severity != model.SeverityCritical {
		t.Errorf("issue = %+v, want open/critical", idx)
	}
	if idx.ImpactPoints != 1000 {
		t.Errorf("impact = %d, want 1000", idx.ImpactPoints)
	}
	if !idx.OpenedAt.Equal(now) || !idx.LastSeenAt.Equal(now) {
		t.Errorf("timestamps wrong: %+v", idx)
	}
}

func TestEngineClosesIssueOnPass(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	fs := newFakeIssueStore()
	// Pre-seed an open indexability issue.
	fs.open[key(5, "indexability_flip")] = model.Issue{URLID: 5, RuleID: "indexability_flip", Status: model.IssueOpen, Severity: model.SeverityCritical}

	eng := NewEngine(DefaultRuleSet(), fs, func() time.Time { return now })
	ctx := EvalContext{
		URLID:      5,
		Importance: 1.0,
		New:        model.Snapshot{Indexable: true, Title: "T", Canonical: "https://x/a", MetaRobots: "index,follow", HTTPStatus: 200, Headings: `{"h1":["x"]}`, MetaDescription: "d"},
		Old:        model.Snapshot{ID: 1, Indexable: true, Title: "T", Canonical: "https://x/a", HTTPStatus: 200},
	}
	if err := eng.Apply(context.Background(), ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	found := false
	for _, c := range fs.closes {
		if c == "indexability_flip" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected indexability_flip closed, closes=%+v", fs.closes)
	}
}

// failingTitleCtx is an EvalContext where only title_changed fails (title empty)
// and everything else passes, for the given URL.
func failingTitleCtx(urlID int64) EvalContext {
	return EvalContext{
		URLID:      urlID,
		Importance: 1.0,
		New: model.Snapshot{
			Indexable: true, Title: "", Canonical: "https://x/a", MetaRobots: "index,follow",
			HTTPStatus: 200, Headings: `{"h1":["x"]}`, MetaDescription: "d",
		},
		Old: model.Snapshot{ID: 1, Indexable: true, Canonical: "https://x/a", HTTPStatus: 200},
	}
}

func TestEnginePreservesOpenedAtOnReFailure(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(48 * time.Hour)

	clock := t0
	fs := newFakeIssueStore()
	eng := NewEngine(DefaultRuleSet(), fs, func() time.Time { return clock })

	// First Apply at t0: title missing -> opens issue with OpenedAt=t0.
	if err := eng.Apply(context.Background(), failingTitleCtx(7)); err != nil {
		t.Fatalf("Apply t0: %v", err)
	}
	first, ok := fs.open[key(7, "title_changed")]
	if !ok {
		t.Fatalf("expected title_changed opened at t0, open=%+v", fs.open)
	}
	if !first.OpenedAt.Equal(t0) || !first.LastSeenAt.Equal(t0) {
		t.Fatalf("first open timestamps = opened %v / seen %v, want both %v", first.OpenedAt, first.LastSeenAt, t0)
	}

	// Second Apply at t1, still failing: must NOT re-open. OpenedAt stays t0,
	// LastSeenAt advances to t1.
	clock = t1
	if err := eng.Apply(context.Background(), failingTitleCtx(7)); err != nil {
		t.Fatalf("Apply t1: %v", err)
	}
	second := fs.open[key(7, "title_changed")]
	if !second.OpenedAt.Equal(t0) {
		t.Errorf("OpenedAt = %v, want preserved %v (no re-open)", second.OpenedAt, t0)
	}
	if !second.LastSeenAt.Equal(t1) {
		t.Errorf("LastSeenAt = %v, want advanced to %v", second.LastSeenAt, t1)
	}
}

func TestEngineIsolatesIssuesPerURL(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	fs := newFakeIssueStore()
	eng := NewEngine(DefaultRuleSet(), fs, func() time.Time { return now })

	// Same rule (title_changed) fails on two different URLs.
	if err := eng.Apply(context.Background(), failingTitleCtx(11)); err != nil {
		t.Fatalf("Apply url 11: %v", err)
	}
	if err := eng.Apply(context.Background(), failingTitleCtx(22)); err != nil {
		t.Fatalf("Apply url 22: %v", err)
	}

	i11, ok11 := fs.open[key(11, "title_changed")]
	i22, ok22 := fs.open[key(22, "title_changed")]
	if !ok11 || !ok22 {
		t.Fatalf("expected two distinct issues, open=%+v", fs.open)
	}
	if i11.URLID != 11 || i22.URLID != 22 {
		t.Errorf("issues not isolated per URL: %+v / %+v", i11, i22)
	}
	if len(fs.open) != 2 {
		t.Errorf("want 2 distinct issues keyed by (urlID,ruleID), got %d: %+v", len(fs.open), fs.open)
	}
}

func TestEngineDoesNotReopenIgnored(t *testing.T) {
	now := time.Now()
	fs := newFakeIssueStore()
	fs.open[key(5, "title_changed")] = model.Issue{URLID: 5, RuleID: "title_changed", Status: model.IssueIgnored}
	eng := NewEngine(DefaultRuleSet(), fs, func() time.Time { return now })
	ctx := EvalContext{URLID: 5, Importance: 1, New: model.Snapshot{Title: "", Indexable: true, Canonical: "c", HTTPStatus: 200, Headings: "h1", MetaDescription: "d", MetaRobots: "index"}}
	if err := eng.Apply(context.Background(), ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, u := range fs.upserts {
		if u.RuleID == "title_changed" {
			t.Errorf("ignored issue must not be re-upserted as open: %+v", u)
		}
	}
}

// TestEngineClosesIgnoredOnPass covers F36: an issue the user silenced while it
// was failing must be cleared once the underlying problem is actually fixed —
// otherwise it lingers as 'ignored' forever and silently suppresses a later
// genuine recurrence. When the rule now PASSES, the engine must issue a close for
// the ignored issue (transition it out of 'ignored'); it must NOT re-upsert it as
// open.
func TestEngineClosesIgnoredOnPass(t *testing.T) {
	now := time.Now()
	fs := newFakeIssueStore()
	// User silenced title_changed while the title was missing.
	fs.open[key(5, "title_changed")] = model.Issue{URLID: 5, RuleID: "title_changed", Status: model.IssueIgnored}
	eng := NewEngine(DefaultRuleSet(), fs, func() time.Time { return now })

	// The title is now present, so title_changed passes.
	ctx := EvalContext{
		URLID:      5,
		Importance: 1,
		New: model.Snapshot{
			Title: "Fixed", Indexable: true, Canonical: "c", HTTPStatus: 200,
			Headings: `{"h1":["x"]}`, MetaDescription: "d", MetaRobots: "index",
		},
		Old: model.Snapshot{ID: 1, Indexable: true, Canonical: "c", HTTPStatus: 200},
	}
	if err := eng.Apply(context.Background(), ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	closed := false
	for _, c := range fs.closes {
		if c == "title_changed" {
			closed = true
		}
	}
	if !closed {
		t.Errorf("ignored-then-recovered issue must be closed, closes=%+v", fs.closes)
	}
	for _, u := range fs.upserts {
		if u.RuleID == "title_changed" {
			t.Errorf("recovered ignored issue must not be re-upserted as open: %+v", u)
		}
	}
}
