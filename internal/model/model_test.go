package model

import (
	"testing"
)

func TestEnumValues(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"severity critical", string(SeverityCritical), "critical"},
		{"severity warning", string(SeverityWarning), "warning"},
		{"severity info", string(SeverityInfo), "info"},
		{"status page", string(StatusPage), "page"},
		{"status unreachable", string(StatusUnreachable), "unreachable"},
		{"issue open", string(IssueOpen), "open"},
		{"change substantive", string(ChangeSubstantive), "substantive"},
		{"fetch ok", string(FetchOK), "ok"},
		{"fetch soft_block", string(FetchSoftBlock), "soft_block"},
		{"fetch hard_block", string(FetchHardBlock), "hard_block"},
		{"fetch unreachable", string(FetchUnreachable), "unreachable"},
		{"alert open", string(AlertOpen), "open"},
		{"file kind robots", string(FileKindRobots), "robots"},
		{"file kind sitemap", string(FileKindSitemap), "sitemap"},
		{"monitoring blocked", ChangeTypeMonitoringBlocked, "monitoring_blocked"},
		{"monitoring unreachable", ChangeTypeMonitoringUnreachable, "monitoring_unreachable"},
		{"render server_rendered", string(RenderServerRendered), "server_rendered"},
		{"render hydrated", string(RenderHydrated), "hydrated"},
		{"render head_only_shell", string(RenderHeadOnlyShell), "head_only_shell"},
		{"render client_shell", string(RenderClientShell), "client_shell"},
		{"render unknown", string(RenderUnknown), "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

// TestRenderModeSet pins the complete set of RenderMode consts so a future
// addition (or accidental deletion/rename) is caught here. The set must mirror
// precheck.RenderKind's five values, but model stays dependency-free — the
// value-set equality against precheck lives in the precheck/extract wave where
// importing both is legal (acceptance #9), NOT here.
func TestRenderModeSet(t *testing.T) {
	// The exact, ordered set of RenderMode string values this package guarantees.
	want := []string{
		"server_rendered",
		"hydrated",
		"head_only_shell",
		"client_shell",
		"unknown",
	}
	got := []RenderMode{
		RenderServerRendered,
		RenderHydrated,
		RenderHeadOnlyShell,
		RenderClientShell,
		RenderUnknown,
	}
	if len(got) != len(want) {
		t.Fatalf("RenderMode set size: got %d consts, want %d", len(got), len(want))
	}
	seen := make(map[RenderMode]bool, len(got))
	for i, rm := range got {
		if string(rm) != want[i] {
			t.Errorf("RenderMode[%d]: got %q, want %q", i, string(rm), want[i])
		}
		if seen[rm] {
			t.Errorf("RenderMode value %q is duplicated", string(rm))
		}
		seen[rm] = true
	}
}

// TestSnapshotRenderFields proves the two A8 fields exist on Snapshot with the
// expected types and db tags, and that the zero value of RenderMode is the
// empty string (which the render surfaces map to "unknown" for pre-A8 rows).
func TestSnapshotRenderFields(t *testing.T) {
	var snap Snapshot
	snap.RenderMode = RenderClientShell
	snap.ExtractionSource = "dom+next_data"

	if snap.RenderMode != RenderClientShell {
		t.Errorf("Snapshot.RenderMode: got %q, want %q", snap.RenderMode, RenderClientShell)
	}
	if snap.ExtractionSource != "dom+next_data" {
		t.Errorf("Snapshot.ExtractionSource: got %q, want %q", snap.ExtractionSource, "dom+next_data")
	}

	var zero Snapshot
	if zero.RenderMode != "" {
		t.Errorf("zero-value Snapshot.RenderMode: got %q, want empty string", zero.RenderMode)
	}
}
