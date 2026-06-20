package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/diff"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

func TestReportRender_Table(t *testing.T) {
	t.Parallel()
	res := store.ReportResult{
		Changes: store.ChangeSummary{Total: 5, Substantive: 4, Cosmetic: 1},
		Issues:  store.IssueSummary{OpenTotal: 3, OpenCritical: 1, OpenWarning: 1, OpenInfo: 1, OpenedInWindow: 3, ClosedInWindow: 1},
		TopURLs: []store.URLChangeCount{{URLID: 10, URL: "https://a.test/p1", Count: 3, LastChanged: time.Date(2026, 6, 9, 11, 0, 0, 0, time.UTC)}},
		Sites:   []store.SiteRollup{{SiteID: 1, BaseURL: "https://a.test", Changes: 4, OpenIssues: 2}},
	}
	var buf bytes.Buffer
	since := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	if err := renderReportTable(&buf, res, since, until, nil); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"CHANGES", "total 5", "substantive 4", "ISSUES", "open now 3", "opened 3", "closed 1", "https://a.test/p1", "all sites"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q in:\n%s", want, out)
		}
	}
}

func TestReportRender_JSON(t *testing.T) {
	t.Parallel()
	res := store.ReportResult{Changes: store.ChangeSummary{Total: 2, Substantive: 2}}
	var buf bytes.Buffer
	since := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	if err := renderReportJSON(&buf, res, since, until, nil); err != nil {
		t.Fatalf("json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, buf.String())
	}
	if got["since"] != "2026-06-02T12:00:00Z" || got["until"] != "2026-06-09T12:00:00Z" {
		t.Fatalf("json window = %v / %v", got["since"], got["until"])
	}
	ch := got["changes"].(map[string]any)
	if ch["total"].(float64) != 2 {
		t.Fatalf("json changes.total = %v", ch["total"])
	}
}

// TestReportRender_SiteScoped exercises the non-nil siteID branch that the
// all-sites tests above never hit: the "site N" scope label in the table render
// and the reportJSON.SiteID field (with its omitempty tag) in the JSON render.
func TestReportRender_SiteScoped(t *testing.T) {
	t.Parallel()
	res := store.ReportResult{Changes: store.ChangeSummary{Total: 1, Substantive: 1}}
	since := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	id := int64(7)

	var table bytes.Buffer
	if err := renderReportTable(&table, res, since, until, &id); err != nil {
		t.Fatalf("render table: %v", err)
	}
	if !strings.Contains(table.String(), "site 7") {
		t.Fatalf("table missing scope label %q in:\n%s", "site 7", table.String())
	}

	var jb bytes.Buffer
	if err := renderReportJSON(&jb, res, since, until, &id); err != nil {
		t.Fatalf("render json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(jb.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, jb.String())
	}
	sid, ok := got["site_id"]
	if !ok || sid.(float64) != 7 {
		t.Fatalf("json site_id = %v (present=%v), want 7", sid, ok)
	}
}

// TestReportCountsRenderModeChange is the SIBLING-SWITCH guard for A8: a render_mode
// flip is a new strField in diff.Compare (classified ChangeSubstantive), and the
// report change-total counting must pick it up. store.BuildReport -> changeSummary
// groups by change_class ONLY (no per-field switch), so a render_mode change flows
// into Total/Substantive automatically — this test PROVES that end to end rather than
// trusting the claim. It drives the real diff -> store -> report path:
//
//	(1) diff.Compare produces the render_mode change (using the EXACT encoding the
//	    production diff emits — guarding against a silent reclassification),
//	(2) RecordChanges persists it as the stored change row, and
//	(3) BuildReport aggregates it into the windowed Changes summary.
//
// A regression that dropped render_mode from diff's strFields, or that special-cased
// it out of the change-class rollup, would make Total/Substantive fall back to 0.
func TestReportCountsRenderModeChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rmrep.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Date(2026, 6, 13, 11, 0, 0, 0, time.UTC)
	siteID, err := db.AddSite(ctx, model.Site{BaseURL: "https://rep.test", Name: "Rep", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	urlID, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://rep.test/p", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}

	// Two snapshots that differ ONLY in render_mode, persisted so the changes' FK
	// (changes.snapshot_id -> snapshots.id) is satisfiable. An identical ContentSHA256
	// on both keeps the body unchanged so render_mode is the sole emitted diff; a
	// non-zero old.ID clears diff.Compare's baseline guard so the flip is a real change.
	oldID, err := db.SaveSnapshot(ctx, model.Snapshot{URLID: urlID, FetchedAt: now, HTTPStatus: 200, ContentSHA256: "abc", RenderMode: model.RenderHydrated})
	if err != nil {
		t.Fatalf("SaveSnapshot(old): %v", err)
	}
	newID, err := db.SaveSnapshot(ctx, model.Snapshot{URLID: urlID, FetchedAt: now, HTTPStatus: 200, ContentSHA256: "abc", RenderMode: model.RenderClientShell})
	if err != nil {
		t.Fatalf("SaveSnapshot(new): %v", err)
	}
	old := model.Snapshot{ID: oldID, URLID: urlID, ContentSHA256: "abc", RenderMode: model.RenderHydrated}
	newSnap := model.Snapshot{ID: newID, URLID: urlID, ContentSHA256: "abc", RenderMode: model.RenderClientShell}
	changes := diff.Compare(newSnap, old, 3, now)

	// Guard the precondition: diff must actually emit the render_mode flip (and only
	// that field, since the snapshots are otherwise identical), classified
	// substantive. If this slips, the report assertion below would be vacuous.
	var sawRenderMode bool
	for _, c := range changes {
		if c.Field == "render_mode" {
			sawRenderMode = true
			if c.ChangeClass != model.ChangeSubstantive {
				t.Fatalf("render_mode change_class = %q, want substantive", c.ChangeClass)
			}
			if c.OldValue != string(model.RenderHydrated) || c.NewValue != string(model.RenderClientShell) {
				t.Fatalf("render_mode change values = %q -> %q, want hydrated -> client_shell", c.OldValue, c.NewValue)
			}
		}
	}
	if !sawRenderMode {
		t.Fatalf("diff.Compare emitted no render_mode change; got %+v", changes)
	}

	if err := db.RecordChanges(ctx, changes); err != nil {
		t.Fatalf("RecordChanges: %v", err)
	}

	res, err := db.BuildReport(ctx, store.ReportParams{Since: now.Add(-time.Hour), SiteID: &siteID, TopN: 10})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if res.Changes.Substantive < 1 {
		t.Fatalf("render_mode flip not counted as substantive: Changes=%+v", res.Changes)
	}
	if res.Changes.Total < 1 {
		t.Fatalf("render_mode flip not counted in Total: Changes=%+v", res.Changes)
	}
	// The invariant the report documents: Total == Substantive + Cosmetic.
	if res.Changes.Total != res.Changes.Substantive+res.Changes.Cosmetic {
		t.Fatalf("Total invariant broken: %+v", res.Changes)
	}
}

func TestReportCmd_BadFlags(t *testing.T) {
	t.Parallel()
	// Bind each case to its intended CLI-layer validation message so a future
	// refactor that moved validation after loadConfig/store.Open (which can fail
	// for unrelated reasons) can't let this test pass for the wrong reason.
	cases := []struct {
		args     []string
		wantSubs string
	}{
		{args: []string{"report", "--since", "nope"}, wantSubs: "--since"},
		{args: []string{"report", "--limit", "0"}, wantSubs: "--limit"},
	}
	for _, tc := range cases {
		cmd := NewRootCmd(BuildInfo{})
		cmd.SetArgs(tc.args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("args %v: expected error", tc.args)
		}
		if !strings.Contains(err.Error(), tc.wantSubs) {
			t.Fatalf("args %v: error %q does not mention %q (must fail at CLI validation)", tc.args, err, tc.wantSubs)
		}
	}
}
