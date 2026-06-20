package wizard

import (
	"strings"
	"testing"
)

// TestMenuGrafanaMapping pins criterion 9's pure mapping: the new "See it on a
// dashboard" menu row maps to the UpgradeGrafana action and re-enters the menu
// (run=true, finish=false) — exactly like the other upgrade rows.
func TestMenuGrafanaMapping(t *testing.T) {
	action, run, finish := menuStep(MenuGrafana)
	if finish {
		t.Fatalf("menuStep(MenuGrafana) finish = true, want false (it runs an action)")
	}
	if !run {
		t.Fatalf("menuStep(MenuGrafana) run = false, want true")
	}
	if action != UpgradeGrafana {
		t.Fatalf("menuStep(MenuGrafana) action = %v, want UpgradeGrafana", action)
	}
}

// TestMenuFinishStillFinishesAfterGrafana guards that adding MenuGrafana before
// MenuFinish did not shift the "finish" semantics: MenuFinish (and any unknown
// value) must still end the loop.
func TestMenuFinishStillFinishesAfterGrafana(t *testing.T) {
	if _, run, finish := menuStep(MenuFinish); run || !finish {
		t.Fatalf("menuStep(MenuFinish) = run %v finish %v, want run=false finish=true", run, finish)
	}
}

// TestGrafanaSizingCopy_SettledWording pins the SETTLED sizing wording shown in
// the huh Note BEFORE the path choice (criterion 9): it must contain "512 MB" and
// "2 GB" so the operator sees the real footprint before committing.
func TestGrafanaSizingCopy_SettledWording(t *testing.T) {
	if !strings.Contains(GrafanaSizingNote, "512 MB") {
		t.Fatalf("GrafanaSizingNote %q must contain \"512 MB\"", GrafanaSizingNote)
	}
	if !strings.Contains(GrafanaSizingNote, "2 GB") {
		t.Fatalf("GrafanaSizingNote %q must contain \"2 GB\"", GrafanaSizingNote)
	}
	// Honest "1 GB fits but snug" beat from the settled copy.
	if !strings.Contains(GrafanaSizingNote, "1 GB") {
		t.Fatalf("GrafanaSizingNote %q must keep the \"1 GB fits but snug\" beat", GrafanaSizingNote)
	}
}
