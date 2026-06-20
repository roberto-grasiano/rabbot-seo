package rules

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/serpwidth"
)

// overflowDetail is the shape of the Detail JSON the two SERP-fit overflow rules
// emit. The spec pins it to exactly {measured_px, budget_px, chars}.
type overflowDetail struct {
	MeasuredPx int `json:"measured_px"`
	BudgetPx   int `json:"budget_px"`
	Chars      int `json:"chars"`
}

// TestSerpFitConstants pins the exported budget/font constants to the spec values
// so a silent edit to the policy numbers is caught. Strictly-greater fires;
// exactly-on-budget fits (asserted in the rule tests below).
func TestSerpFitConstants(t *testing.T) {
	if TitleFontPx != 20 {
		t.Errorf("TitleFontPx = %d, want 20", TitleFontPx)
	}
	if TitleBudgetPx != 580 {
		t.Errorf("TitleBudgetPx = %d, want 580", TitleBudgetPx)
	}
	if DescriptionFontPx != 14 {
		t.Errorf("DescriptionFontPx = %d, want 14", DescriptionFontPx)
	}
	if DescriptionBudgetPx != 920 {
		t.Errorf("DescriptionBudgetPx = %d, want 920", DescriptionBudgetPx)
	}
}

// TestTitlePixelOverflowRule covers A3 acceptance #5 (title half): an over-budget
// title fails Warning with a Detail that unmarshals to exactly
// {measured_px, budget_px, chars} with budget_px == TitleBudgetPx; an under-budget
// title passes; and an EMPTY title passes (missing is title_changed's job — no
// double-fire).
func TestTitlePixelOverflowRule(t *testing.T) {
	r := titlePixelOverflowRule{}

	tests := []struct {
		name     string
		title    string
		wantFail bool
	}{
		// 48×'W' @20px ≈ 906px > 580 budget (the spec's worked example).
		{"wide title overflows", strings.Repeat("W", 48), true},
		// 70×'i' @20px ≈ 44px < 580 budget — more chars, fits. The thesis.
		{"narrow title fits despite many chars", strings.Repeat("i", 70), false},
		{"empty title passes (no double-fire with title_changed)", "", false},
		{"whitespace-only title passes", "   ", false},
		{"short title fits", "Home", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := r.Eval(EvalContext{New: model.Snapshot{Title: tc.title}})
			if f.RuleID != "title_pixel_overflow" {
				t.Errorf("RuleID = %q, want title_pixel_overflow", f.RuleID)
			}
			if f.Failed != tc.wantFail {
				t.Fatalf("title=%q: Failed=%v, want %v (%+v)", tc.title, f.Failed, tc.wantFail, f)
			}
			if !tc.wantFail {
				return
			}
			if f.Severity != model.SeverityWarning {
				t.Errorf("overflow must be warning, got %v", f.Severity)
			}
			var d overflowDetail
			if err := json.Unmarshal([]byte(f.Detail), &d); err != nil {
				t.Fatalf("Detail %q does not unmarshal: %v", f.Detail, err)
			}
			if d.BudgetPx != TitleBudgetPx {
				t.Errorf("budget_px = %d, want %d", d.BudgetPx, TitleBudgetPx)
			}
			if d.Chars != len([]rune(tc.title)) {
				t.Errorf("chars = %d, want %d", d.Chars, len([]rune(tc.title)))
			}
			// measured_px is the rounded rendered width; must exceed the budget.
			if d.MeasuredPx <= TitleBudgetPx {
				t.Errorf("measured_px = %d, want > %d", d.MeasuredPx, TitleBudgetPx)
			}
			// Pin against serpwidth so the rule and the measurer agree.
			wantMeasured := int(serpwidth.Width(tc.title, TitleFontPx) + 0.5)
			if d.MeasuredPx != wantMeasured {
				t.Errorf("measured_px = %d, want %d (serpwidth rounded)", d.MeasuredPx, wantMeasured)
			}
		})
	}
}

// TestTitlePixelOverflowExactExample pins the spec's worked example end-to-end:
// 48×'W' yields measured_px:906, budget_px:580, chars:48.
func TestTitlePixelOverflowExactExample(t *testing.T) {
	f := titlePixelOverflowRule{}.Eval(EvalContext{New: model.Snapshot{Title: strings.Repeat("W", 48)}})
	if !f.Failed {
		t.Fatalf("48×W must overflow, got %+v", f)
	}
	var d overflowDetail
	if err := json.Unmarshal([]byte(f.Detail), &d); err != nil {
		t.Fatalf("Detail %q: %v", f.Detail, err)
	}
	if d.MeasuredPx != 906 || d.BudgetPx != 580 || d.Chars != 48 {
		t.Errorf("Detail = %+v, want {906, 580, 48}", d)
	}

	// Acceptance #5 says the Detail unmarshals to EXACTLY {measured_px, budget_px,
	// chars}. A struct unmarshal silently ignores unknown/renamed keys, so it would
	// NOT catch a 4th field or a rename. Pin the field SET via a map so the "exactly"
	// is enforced at the rule level: precisely these three keys, no more, no less.
	var raw map[string]any
	if err := json.Unmarshal([]byte(f.Detail), &raw); err != nil {
		t.Fatalf("Detail %q does not unmarshal to a map: %v", f.Detail, err)
	}
	wantKeys := map[string]bool{"measured_px": true, "budget_px": true, "chars": true}
	if len(raw) != len(wantKeys) {
		t.Errorf("Detail has %d keys %v, want exactly %d %v", len(raw), keysOf(raw), len(wantKeys), []string{"measured_px", "budget_px", "chars"})
	}
	for k := range raw {
		if !wantKeys[k] {
			t.Errorf("Detail has unexpected key %q; the field set must be exactly {measured_px, budget_px, chars}", k)
		}
	}
}

// keysOf returns the keys of a map in a stable-enough form for test error messages.
func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestMetaDescriptionPixelOverflowRule covers A3 acceptance #5 (description half)
// at 14px / DescriptionBudgetPx.
func TestMetaDescriptionPixelOverflowRule(t *testing.T) {
	r := metaDescriptionPixelOverflowRule{}

	tests := []struct {
		name     string
		desc     string
		wantFail bool
	}{
		// 70×'W' @14px ≈ 925px > 920 budget.
		{"wide description overflows", strings.Repeat("W", 70), true},
		// 10×'a' @14px ≈ 78px < 920 budget.
		{"short description fits", strings.Repeat("a", 10), false},
		{"empty description passes (no double-fire)", "", false},
		{"whitespace-only description passes", "  \t ", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := r.Eval(EvalContext{New: model.Snapshot{MetaDescription: tc.desc}})
			if f.RuleID != "meta_description_pixel_overflow" {
				t.Errorf("RuleID = %q, want meta_description_pixel_overflow", f.RuleID)
			}
			if f.Failed != tc.wantFail {
				t.Fatalf("desc=%q: Failed=%v, want %v (%+v)", tc.desc, f.Failed, tc.wantFail, f)
			}
			if !tc.wantFail {
				return
			}
			if f.Severity != model.SeverityWarning {
				t.Errorf("overflow must be warning, got %v", f.Severity)
			}
			var d overflowDetail
			if err := json.Unmarshal([]byte(f.Detail), &d); err != nil {
				t.Fatalf("Detail %q does not unmarshal: %v", f.Detail, err)
			}
			if d.BudgetPx != DescriptionBudgetPx {
				t.Errorf("budget_px = %d, want %d", d.BudgetPx, DescriptionBudgetPx)
			}
			if d.Chars != len([]rune(tc.desc)) {
				t.Errorf("chars = %d, want %d", d.Chars, len([]rune(tc.desc)))
			}
			if d.MeasuredPx <= DescriptionBudgetPx {
				t.Errorf("measured_px = %d, want > %d", d.MeasuredPx, DescriptionBudgetPx)
			}
		})
	}
}

// TestSerpFitRulesRegistered covers A3 acceptance #6: both new rule IDs are in
// DefaultRuleSet().
func TestSerpFitRulesRegistered(t *testing.T) {
	have := make(map[string]bool)
	for _, r := range DefaultRuleSet() {
		have[r.ID()] = true
	}
	for _, id := range []string{"title_pixel_overflow", "meta_description_pixel_overflow"} {
		if !have[id] {
			t.Errorf("DefaultRuleSet missing %q", id)
		}
	}
}

// TestEngineReconcilesTitlePixelOverflow covers A3 acceptance #7: an overflowing
// title opens a title_pixel_overflow issue, and a shortened title on the next eval
// closes it (status closed, ClosedAt set — modeled here by the fake's removal of
// the open issue and a recorded close).
func TestEngineReconcilesTitlePixelOverflow(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	fs := newFakeIssueStore()
	eng := NewEngine(DefaultRuleSet(), fs, func() time.Time { return now })

	// Open: an overflowing 48×'W' title (the other default rules pass here).
	overflowing := reconcileBaseSnapshot()
	overflowing.Title = strings.Repeat("W", 48)
	if err := eng.Apply(context.Background(), EvalContext{URLID: 8, Importance: 1, New: overflowing, Old: model.Snapshot{}}); err != nil {
		t.Fatalf("Apply open: %v", err)
	}
	if _, ok := fs.open[key(8, "title_pixel_overflow")]; !ok {
		t.Fatalf("expected title_pixel_overflow open, open=%+v", fs.open)
	}

	// Close: a short title fits, so the issue closes.
	shortened := reconcileBaseSnapshot()
	shortened.Title = "Home"
	if err := eng.Apply(context.Background(), EvalContext{URLID: 8, Importance: 1, New: shortened, Old: model.Snapshot{ID: 1}}); err != nil {
		t.Fatalf("Apply close: %v", err)
	}
	if _, ok := fs.open[key(8, "title_pixel_overflow")]; ok {
		t.Errorf("title_pixel_overflow should close when title fits")
	}
	closed := false
	for _, c := range fs.closes {
		if c == "title_pixel_overflow" {
			closed = true
		}
	}
	if !closed {
		t.Errorf("expected a close recorded for title_pixel_overflow, closes=%+v", fs.closes)
	}
}
