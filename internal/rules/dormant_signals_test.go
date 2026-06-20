package rules

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// TestRedirectChainInfo covers A5 acceptance #4: depth math (len−1, floor 0),
// loop detection, and ok=false on ""/non-JSON.
func TestRedirectChainInfo(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantDepth int
		wantLoop  string
		wantOK    bool
	}{
		{"empty string not ok", "", 0, "", false},
		{"json null not ok", "null", 0, "", false},
		{"garbage not ok", "{not json", 0, "", false},
		{"non-array json not ok", `{"a":1}`, 0, "", false},
		{"empty array depth 0", "[]", 0, "", true},
		{"single hop depth 0", `["https://a"]`, 0, "", true},
		{"two hops depth 1 no loop", `["https://a","https://b"]`, 1, "", true},
		{"three hops depth 2 no loop", `["https://a","https://b","https://c"]`, 2, "", true},
		{"loop A,B,A reports repeated URL", `["https://a","https://b","https://a"]`, 2, "https://a", true},
		{"immediate self loop", `["https://a","https://a"]`, 1, "https://a", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			depth, loopURL, ok := RedirectChainInfo(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("RedirectChainInfo(%q) ok=%v, want %v", tc.in, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if depth != tc.wantDepth {
				t.Errorf("depth = %d, want %d", depth, tc.wantDepth)
			}
			if loopURL != tc.wantLoop {
				t.Errorf("loopURL = %q, want %q", loopURL, tc.wantLoop)
			}
		})
	}
}

// TestExternalLinkSpikeRule covers A5 acceptance #3 (external link half):
// new−old ≥ 10 AND new ≥ 2×old, guarded by Old.ID != 0.
func TestExternalLinkSpikeRule(t *testing.T) {
	r := externalLinkSpikeRule{}
	tests := []struct {
		name     string
		oldID    int64
		oldCount int
		newCount int
		wantFail bool
	}{
		{"first crawl never fires", 0, 0, 1000, false},
		{"0 to 9 passes (under abs floor)", 1, 0, 9, false},
		{"0 to 10 fires (abs floor met, 2x trivially)", 1, 0, 10, true},
		{"100 to 115 passes (abs met but under 2x)", 1, 100, 115, false},
		{"20 to 45 fires (abs 25 and >=2x=40)", 1, 20, 45, true},
		{"20 to 40 fires (exactly 2x, delta 20)", 1, 20, 40, true},
		{"20 to 39 passes (under 2x)", 1, 20, 39, false},
		{"5 to 14 passes (delta 9 under floor)", 1, 5, 14, false},
		{"5 to 15 fires (delta 10 and 3x)", 1, 5, 15, true},
		{"links dropped passes", 1, 100, 10, false},
		{"unchanged passes", 1, 50, 50, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := r.Eval(EvalContext{
				New: model.Snapshot{ExternalLinkCount: tc.newCount},
				Old: model.Snapshot{ID: tc.oldID, ExternalLinkCount: tc.oldCount},
			})
			if f.RuleID != "external_link_spike" {
				t.Errorf("RuleID = %q", f.RuleID)
			}
			if f.Failed != tc.wantFail {
				t.Fatalf("old=%d new=%d (oldID=%d): Failed=%v, want %v", tc.oldCount, tc.newCount, tc.oldID, f.Failed, tc.wantFail)
			}
			if tc.wantFail {
				if f.Severity != model.SeverityWarning {
					t.Errorf("spike must be warning, got %v", f.Severity)
				}
				assertOldNewDetail(t, f.Detail, tc.oldCount, tc.newCount)
			}
		})
	}
}

// TestImageAltRegressionRule covers A5 acceptance #3 (alt regression half):
// fires only when New.MissingAltCount > Old.MissingAltCount, guarded Old.ID != 0.
func TestImageAltRegressionRule(t *testing.T) {
	r := imageAltRegressionRule{}
	tests := []struct {
		name     string
		oldID    int64
		oldMiss  int
		newMiss  int
		wantFail bool
	}{
		{"first crawl never fires", 0, 0, 50, false},
		{"missing increased fires", 1, 2, 5, true},
		{"missing by one fires", 1, 0, 1, true},
		{"missing unchanged passes", 1, 3, 3, false},
		{"missing decreased passes (the re-baseline case)", 1, 5, 2, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := r.Eval(EvalContext{
				New: model.Snapshot{MissingAltCount: tc.newMiss},
				Old: model.Snapshot{ID: tc.oldID, MissingAltCount: tc.oldMiss},
			})
			if f.RuleID != "image_alt_regression" {
				t.Errorf("RuleID = %q", f.RuleID)
			}
			if f.Failed != tc.wantFail {
				t.Fatalf("oldMiss=%d newMiss=%d (oldID=%d): Failed=%v, want %v", tc.oldMiss, tc.newMiss, tc.oldID, f.Failed, tc.wantFail)
			}
			if tc.wantFail {
				if f.Severity != model.SeverityWarning {
					t.Errorf("regression must be warning, got %v", f.Severity)
				}
				assertOldNewDetail(t, f.Detail, tc.oldMiss, tc.newMiss)
			}
		})
	}
}

// TestImageAltMissingRule covers A5 acceptance #3 (alt missing half): info-tier
// steady-state rule that fires only when ImageCount ≥ altMinImageCount AND alt
// coverage < altCoverageFloor. No first-crawl guard (steady state).
func TestImageAltMissingRule(t *testing.T) {
	r := imageAltMissingRule{}
	tests := []struct {
		name     string
		images   int
		missing  int
		wantFail bool
	}{
		{"10 images 3 missing fires at 70% coverage", 10, 3, true},
		{"10 images 2 missing passes at exactly 0.80 floor", 10, 2, false},
		{"10 images 1 missing passes (90% coverage)", 10, 1, false},
		{"4 images 4 missing passes under 5-image minimum", 4, 4, false},
		{"0 images passes (no division by zero)", 0, 0, false},
		{"5 images 5 missing fires (0% coverage, meets min)", 5, 5, true},
		{"5 images 1 missing passes (80% coverage at floor)", 5, 1, false},
		{"100 images 21 missing fires (79% coverage)", 100, 21, true},
		{"100 images 20 missing passes (exactly 80%)", 100, 20, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := r.Eval(EvalContext{New: model.Snapshot{ImageCount: tc.images, MissingAltCount: tc.missing}})
			if f.RuleID != "image_alt_missing" {
				t.Errorf("RuleID = %q", f.RuleID)
			}
			if f.Failed != tc.wantFail {
				t.Fatalf("images=%d missing=%d: Failed=%v, want %v", tc.images, tc.missing, f.Failed, tc.wantFail)
			}
			if tc.wantFail && f.Severity != model.SeverityInfo {
				t.Errorf("alt missing must be info tier, got %v", f.Severity)
			}
		})
	}
}

// TestImageAltMissingConstants pins the floor/min constants per the spec.
func TestImageAltMissingConstants(t *testing.T) {
	if altCoverageFloor != 0.80 {
		t.Errorf("altCoverageFloor = %v, want 0.80", altCoverageFloor)
	}
	if altMinImageCount != 5 {
		t.Errorf("altMinImageCount = %d, want 5", altMinImageCount)
	}
}

// TestRedirectChainGrowthRule covers A5 acceptance #3 (growth half): newDepth >
// oldDepth and new chain has NO loop; guarded Old.ID != 0 and both chains parse.
func TestRedirectChainGrowthRule(t *testing.T) {
	r := redirectChainGrowthRule{}
	tests := []struct {
		name     string
		oldID    int64
		oldChain string
		newChain string
		wantFail bool
	}{
		{"first crawl never fires", 0, "", `["a","b"]`, false},
		{"depth 0 to 1 fires", 1, `["a"]`, `["a","b"]`, true},
		{"depth 2 to 3 fires", 1, `["a","b","c"]`, `["a","b","c","d"]`, true},
		{"depth unchanged passes", 1, `["a","b"]`, `["a","b"]`, false},
		{"depth shrank passes", 1, `["a","b","c"]`, `["a","b"]`, false},
		{"grew but new chain loops yields to loop rule", 1, `["a"]`, `["a","b","a"]`, false},
		{"old chain unparseable passes", 1, "garbage", `["a","b"]`, false},
		{"new chain unparseable passes", 1, `["a"]`, "garbage", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := r.Eval(EvalContext{
				New: model.Snapshot{RedirectChain: tc.newChain},
				Old: model.Snapshot{ID: tc.oldID, RedirectChain: tc.oldChain},
			})
			if f.RuleID != "redirect_chain_growth" {
				t.Errorf("RuleID = %q", f.RuleID)
			}
			if f.Failed != tc.wantFail {
				t.Fatalf("old=%q new=%q (oldID=%d): Failed=%v, want %v", tc.oldChain, tc.newChain, tc.oldID, f.Failed, tc.wantFail)
			}
			if tc.wantFail && f.Severity != model.SeverityWarning {
				t.Errorf("growth must be warning, got %v", f.Severity)
			}
		})
	}
}

// TestRedirectLoopRule covers A5 acceptance #3 (loop half): critical when a URL
// repeats in the new chain; passes on a clean chain; no finding on garbage. No
// Old.ID guard (steady-state rule on the new chain alone).
func TestRedirectLoopRule(t *testing.T) {
	r := redirectLoopRule{}
	tests := []struct {
		name         string
		newChain     string
		wantFail     bool
		wantRepeated string
	}{
		{"clean chain passes", `["https://a","https://b"]`, false, ""},
		{"single hop passes", `["https://a"]`, false, ""},
		{"empty array passes", `[]`, false, ""},
		{"loop A,B,A fires critical", `["https://a","https://b","https://a"]`, true, "https://a"},
		{"immediate loop fires", `["https://a","https://a"]`, true, "https://a"},
		{"empty json no finding", "", false, ""},
		{"garbage json no finding", "{bad", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := r.Eval(EvalContext{New: model.Snapshot{RedirectChain: tc.newChain}})
			if f.RuleID != "redirect_loop" {
				t.Errorf("RuleID = %q", f.RuleID)
			}
			if f.Failed != tc.wantFail {
				t.Fatalf("new=%q: Failed=%v, want %v", tc.newChain, f.Failed, tc.wantFail)
			}
			if tc.wantFail {
				if f.Severity != model.SeverityCritical {
					t.Errorf("loop must be critical, got %v", f.Severity)
				}
				if !strings.Contains(f.Detail, tc.wantRepeated) {
					t.Errorf("detail %q should contain repeated URL %q", f.Detail, tc.wantRepeated)
				}
			}
		})
	}
}

// TestDormantSignalsRegistered covers A5: all five new rule IDs are in DefaultRuleSet().
func TestDormantSignalsRegistered(t *testing.T) {
	have := make(map[string]bool)
	for _, r := range DefaultRuleSet() {
		have[r.ID()] = true
	}
	for _, id := range []string{
		"external_link_spike", "image_alt_regression", "image_alt_missing",
		"redirect_chain_growth", "redirect_loop",
	} {
		if !have[id] {
			t.Errorf("DefaultRuleSet missing %q", id)
		}
	}
}

// assertOldNewDetail checks a {"old":N,"new":M} detail payload.
func assertOldNewDetail(t *testing.T, detail string, wantOld, wantNew int) {
	t.Helper()
	var d struct {
		Old int `json:"old"`
		New int `json:"new"`
	}
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		t.Fatalf("detail %q does not unmarshal: %v", detail, err)
	}
	if d.Old != wantOld || d.New != wantNew {
		t.Errorf("detail = {old:%d,new:%d}, want {old:%d,new:%d}", d.Old, d.New, wantOld, wantNew)
	}
}

// --- Engine reconciliation (A5 acceptance #5) ---

// reconcileBaseSnapshot is a snapshot where the *other* default rules all pass,
// so reconciliation tests isolate the dormant-signal rule under test.
func reconcileBaseSnapshot() model.Snapshot {
	return model.Snapshot{
		Indexable: true, Title: "Stable Title", Canonical: "https://x/a",
		MetaRobots: "index,follow", HTTPStatus: 200, Headings: `{"h1":["x"]}`,
		MetaDescription: "stable copy", RedirectChain: `["https://x/a"]`,
	}
}

// TestEngineReconcilesExternalLinkSpike covers A5 acceptance #5: a fired-then-
// recovered external_link_spike issue closes on the next Apply.
func TestEngineReconcilesExternalLinkSpike(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	fs := newFakeIssueStore()
	eng := NewEngine(DefaultRuleSet(), fs, func() time.Time { return now })

	// Fire: 10 -> 40 external links (delta 30, 4x).
	spikeNew := reconcileBaseSnapshot()
	spikeNew.ExternalLinkCount = 40
	old := reconcileBaseSnapshot()
	old.ID = 1
	old.ExternalLinkCount = 10
	if err := eng.Apply(context.Background(), EvalContext{URLID: 3, Importance: 1, New: spikeNew, Old: old}); err != nil {
		t.Fatalf("Apply fire: %v", err)
	}
	if _, ok := fs.open[key(3, "external_link_spike")]; !ok {
		t.Fatalf("expected external_link_spike open, open=%+v", fs.open)
	}

	// Recover: counts settle (40 -> 40, no spike vs new old of 40).
	settled := reconcileBaseSnapshot()
	settled.ExternalLinkCount = 40
	oldSettled := reconcileBaseSnapshot()
	oldSettled.ID = 2
	oldSettled.ExternalLinkCount = 40
	if err := eng.Apply(context.Background(), EvalContext{URLID: 3, Importance: 1, New: settled, Old: oldSettled}); err != nil {
		t.Fatalf("Apply recover: %v", err)
	}
	if _, ok := fs.open[key(3, "external_link_spike")]; ok {
		t.Errorf("external_link_spike should close on recovery, still open")
	}
}

// TestEngineReconcilesImageAltRegression covers A5 acceptance #5 for alt regression.
func TestEngineReconcilesImageAltRegression(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	fs := newFakeIssueStore()
	eng := NewEngine(DefaultRuleSet(), fs, func() time.Time { return now })

	// Fire: missing alt 1 -> 4.
	bad := reconcileBaseSnapshot()
	bad.ImageCount = 10
	bad.MissingAltCount = 4
	old := reconcileBaseSnapshot()
	old.ID = 1
	old.ImageCount = 10
	old.MissingAltCount = 1
	if err := eng.Apply(context.Background(), EvalContext{URLID: 4, Importance: 1, New: bad, Old: old}); err != nil {
		t.Fatalf("Apply fire: %v", err)
	}
	if _, ok := fs.open[key(4, "image_alt_regression")]; !ok {
		t.Fatalf("expected image_alt_regression open, open=%+v", fs.open)
	}

	// Recover: missing alt stays at 4 (no further increase). Use coverage 100%
	// for the steady-state image_alt_missing rule to also stay quiet.
	fixed := reconcileBaseSnapshot()
	fixed.ImageCount = 10
	fixed.MissingAltCount = 0
	oldFixed := reconcileBaseSnapshot()
	oldFixed.ID = 2
	oldFixed.ImageCount = 10
	oldFixed.MissingAltCount = 4
	if err := eng.Apply(context.Background(), EvalContext{URLID: 4, Importance: 1, New: fixed, Old: oldFixed}); err != nil {
		t.Fatalf("Apply recover: %v", err)
	}
	if _, ok := fs.open[key(4, "image_alt_regression")]; ok {
		t.Errorf("image_alt_regression should close when missing count stops rising")
	}
}

// TestEngineReconcilesImageAltMissing covers A5 acceptance #5: image_alt_missing
// stays open while coverage holds below the floor and closes once coverage
// recovers to the floor (or the page drops under altMinImageCount images).
func TestEngineReconcilesImageAltMissing(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	fs := newFakeIssueStore()
	eng := NewEngine(DefaultRuleSet(), fs, func() time.Time { return now })

	// Open: 10 images, 3 missing (70% coverage < 0.80).
	bad := reconcileBaseSnapshot()
	bad.ImageCount = 10
	bad.MissingAltCount = 3
	if err := eng.Apply(context.Background(), EvalContext{URLID: 9, Importance: 1, New: bad, Old: model.Snapshot{}}); err != nil {
		t.Fatalf("Apply open: %v", err)
	}
	if _, ok := fs.open[key(9, "image_alt_missing")]; !ok {
		t.Fatalf("expected image_alt_missing open, open=%+v", fs.open)
	}

	// Still below floor: stays open.
	if err := eng.Apply(context.Background(), EvalContext{URLID: 9, Importance: 1, New: bad, Old: model.Snapshot{ID: 1, ImageCount: 10, MissingAltCount: 3}}); err != nil {
		t.Fatalf("Apply still-bad: %v", err)
	}
	if _, ok := fs.open[key(9, "image_alt_missing")]; !ok {
		t.Errorf("image_alt_missing should stay open while coverage below floor")
	}

	// Recover coverage to the floor: 10 images, 2 missing (exactly 0.80) closes it.
	good := reconcileBaseSnapshot()
	good.ImageCount = 10
	good.MissingAltCount = 2
	if err := eng.Apply(context.Background(), EvalContext{URLID: 9, Importance: 1, New: good, Old: model.Snapshot{ID: 2, ImageCount: 10, MissingAltCount: 3}}); err != nil {
		t.Fatalf("Apply recover: %v", err)
	}
	if _, ok := fs.open[key(9, "image_alt_missing")]; ok {
		t.Errorf("image_alt_missing should close once coverage recovers to floor")
	}
}

// TestEngineReconcilesRedirectLoop covers A5 acceptance #5: a loop stays open while
// the chain still loops and closes once the chain is clean.
func TestEngineReconcilesRedirectLoop(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	fs := newFakeIssueStore()
	eng := NewEngine(DefaultRuleSet(), fs, func() time.Time { return now })

	looping := reconcileBaseSnapshot()
	looping.RedirectChain = `["https://x/a","https://x/b","https://x/a"]`
	if err := eng.Apply(context.Background(), EvalContext{URLID: 6, Importance: 1, New: looping, Old: model.Snapshot{}}); err != nil {
		t.Fatalf("Apply loop: %v", err)
	}
	if _, ok := fs.open[key(6, "redirect_loop")]; !ok {
		t.Fatalf("expected redirect_loop open, open=%+v", fs.open)
	}

	clean := reconcileBaseSnapshot()
	clean.RedirectChain = `["https://x/a"]`
	if err := eng.Apply(context.Background(), EvalContext{URLID: 6, Importance: 1, New: clean, Old: model.Snapshot{ID: 1}}); err != nil {
		t.Fatalf("Apply clean: %v", err)
	}
	if _, ok := fs.open[key(6, "redirect_loop")]; ok {
		t.Errorf("redirect_loop should close once chain is clean")
	}
}
