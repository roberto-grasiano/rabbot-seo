package cli

import (
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestNewIssuesCmdHasOpenFlag(t *testing.T) {
	cmd := newIssuesCmd()
	if cmd.Flags().Lookup("open") == nil {
		t.Error("issues command must expose --open")
	}
	if cmd.Use != "issues" {
		t.Errorf("Use = %q, want issues", cmd.Use)
	}
}

func TestNewIssuesCmdHasStatusFlag(t *testing.T) {
	cmd := newIssuesCmd()
	if cmd.Flags().Lookup("status") == nil {
		t.Fatal("issues command must expose --status")
	}
}

// TestIssuesStatusFlagRejectsUnknown asserts that an unknown --status value is
// rejected up front (before any config/DB access), with a clear error.
func TestIssuesStatusFlagRejectsUnknown(t *testing.T) {
	cmd := newIssuesCmd()
	cmd.SetArgs([]string{"--status", "bogus"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error for unknown --status value, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q should mention the offending value %q", err.Error(), "bogus")
	}
}

// TestParseIssueStatusAcceptsKnown confirms the three lifecycle values parse and
// that unknown values are rejected.
func TestParseIssueStatusAcceptsKnown(t *testing.T) {
	for _, s := range []string{"open", "closed", "ignored"} {
		if _, err := parseIssueStatus(s); err != nil {
			t.Errorf("parseIssueStatus(%q) unexpected error: %v", s, err)
		}
	}
	if _, err := parseIssueStatus("nope"); err == nil {
		t.Error("parseIssueStatus(\"nope\") should error")
	}
}

// TestRenderIssuesIncludesDetailColumn pins A3 acceptance criterion #10: the
// `rabbot issues` table carries a DETAIL column whose cell is the issue's raw
// detail JSON (e.g. {"measured_px":906,"budget_px":580,"chars":48}), so the
// measured px reaches the pull surface verbatim — not humanized (humanizing is a
// push-surface-only open question, scoped to Slack bodies). An empty/"{}" detail
// renders as an empty cell, never a literal "{}".
func TestRenderIssuesIncludesDetailColumn(t *testing.T) {
	t.Parallel()
	const overflowDetail = `{"measured_px":906,"budget_px":580,"chars":48}`
	issues := []model.Issue{
		{ID: 1, URLID: 7, RuleID: "title_pixel_overflow", Status: model.IssueOpen, Severity: model.SeverityWarning, ImpactPoints: 1, Detail: overflowDetail},
		{ID: 2, URLID: 7, RuleID: "title_changed", Status: model.IssueOpen, Severity: model.SeverityWarning, ImpactPoints: 1, Detail: "{}"},
		{ID: 3, URLID: 8, RuleID: "h1_missing", Status: model.IssueClosed, Severity: model.SeverityInfo, ImpactPoints: 0, Detail: ""},
	}
	var buf strings.Builder
	if err := renderIssues(&buf, issues); err != nil {
		t.Fatalf("renderIssues: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "DETAIL") {
		t.Errorf("issues output header missing the DETAIL column:\n%s", out)
	}
	if !strings.Contains(out, overflowDetail) {
		t.Errorf("issues output missing the raw detail JSON %q:\n%s", overflowDetail, out)
	}
	// The empty/"{}" detail rows must not print a literal "{}" placeholder.
	if strings.Contains(out, "{}") {
		t.Errorf("an empty detail should render as a blank cell, not a literal \"{}\":\n%s", out)
	}
	// Column order: DETAIL is the final column, after IMPACT.
	header := strings.SplitN(out, "\n", 2)[0]
	if idx := strings.Index(header, "IMPACT"); idx < 0 || strings.Index(header, "DETAIL") < idx {
		t.Errorf("DETAIL must be the last column (after IMPACT); header was %q", header)
	}
}

// TestRenderIssuesListsNeedsRendering confirms A8's needs_rendering issue reaches
// the `rabbot issues` pull surface for free: renderIssues (and the ListIssues store
// query behind it) are rule-id-agnostic, so the warning row — including its
// client_shell/head_only_shell detail JSON — renders verbatim with no A8-specific
// code in the issues command. A regression that special-cased rule ids would drop it.
func TestRenderIssuesListsNeedsRendering(t *testing.T) {
	t.Parallel()
	const detail = `{"render_mode":"client_shell","monitored":"fetch_status_only"}`
	issues := []model.Issue{
		{ID: 1, URLID: 7, RuleID: "needs_rendering", Status: model.IssueOpen, Severity: model.SeverityWarning, ImpactPoints: 1, Detail: detail},
	}
	var buf strings.Builder
	if err := renderIssues(&buf, issues); err != nil {
		t.Fatalf("renderIssues: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "needs_rendering") {
		t.Errorf("issues output missing the needs_rendering rule row:\n%s", out)
	}
	if !strings.Contains(out, "client_shell") {
		t.Errorf("issues output missing the needs_rendering detail JSON %q:\n%s", detail, out)
	}
}

func TestNewNotifyTestCmd(t *testing.T) {
	cmd := newNotifyCmd()
	sub, _, err := cmd.Find([]string{"test"})
	if err != nil || sub.Name() != "test" {
		t.Errorf("notify test subcommand missing: %v", err)
	}
}

// TestIssueIgnoreRejectsTrailingGarbage reproduces C1: `issue ignore 12abc`
// must fail at id-parse time rather than silently parsing 12. The parse happens
// before any control-client connection, so an invalid id surfaces a clear
// "invalid issue id" error regardless of daemon availability.
func TestIssueIgnoreRejectsTrailingGarbage(t *testing.T) {
	cmd := newIssueCmd()
	cmd.SetArgs([]string{"ignore", "12abc"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error for non-numeric id with trailing garbage, got nil")
	}
	if !strings.Contains(err.Error(), "invalid issue id") || !strings.Contains(err.Error(), "12abc") {
		t.Errorf("error %q should report an invalid issue id mentioning %q", err.Error(), "12abc")
	}
}
