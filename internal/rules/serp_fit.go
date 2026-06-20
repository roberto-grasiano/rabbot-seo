package rules

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/serpwidth"
)

// SERP-fit budgets and reference font sizes (A3). These are POLICY: how wide a
// title or description may render before a search result truncates it. The
// mechanism — measuring rendered pixel width against the desktop reference font —
// lives in internal/serpwidth; these constants live here, next to the rules that
// apply them. Strictly-greater fires; a width exactly on budget fits.
//
// The reference is the DESKTOP SERP only. Google truncates by rendered pixel
// width, not character count: the title container is ~580px of Arial 20px and the
// snippet ~Arial 14px, so a few wide characters can clip where many thin ones fit.
// Mobile renders Roboto in device-dependent containers and is deliberately out of
// scope (see the A3 spec). Per-rule overrides are the F4 fast-follow; these are
// fixed constants today.
const (
	// TitleFontPx is the desktop SERP title font size (Arial 20px).
	TitleFontPx = 20
	// TitleBudgetPx is the desktop SERP title container width in pixels (~580px).
	TitleBudgetPx = 580
	// DescriptionFontPx is the desktop SERP snippet font size (Arial 14px).
	DescriptionFontPx = 14
	// DescriptionBudgetPx is the desktop SERP snippet width in pixels. Tools
	// disagree (920 vs 990); 920 is the conservative desktop reference used here.
	DescriptionBudgetPx = 920
)

// overflowDetailJSON builds the {measured_px, budget_px, chars} detail payload for
// an overflow finding, in the spec's exact field order. measured_px is the
// rendered width rounded to the nearest pixel; chars is the rune count of the
// measured text (informational — character count is NOT what decides fit, which is
// the whole point of these rules).
func overflowDetailJSON(measuredPx float64, budgetPx int, text string) string {
	measured := int(measuredPx + 0.5)
	chars := utf8.RuneCountInString(text)
	return `{"measured_px":` + strconv.Itoa(measured) +
		`,"budget_px":` + strconv.Itoa(budgetPx) +
		`,"chars":` + strconv.Itoa(chars) + `}`
}

// title_pixel_overflow — the title renders wider than the desktop SERP title
// container, so it will truncate in results. Warning.
//
// An empty (or whitespace-only) title PASSES here: a missing title is
// title_changed's finding (default_rules.go), so firing here too would double-fire
// on the same defect. We fail strictly when Width(title) > TitleBudgetPx; a title
// exactly on budget fits. Eval is pure: it measures EvalContext.New.Title directly
// (serpwidth collapses interior whitespace the way the browser lays it out).
type titlePixelOverflowRule struct{}

func (titlePixelOverflowRule) ID() string { return "title_pixel_overflow" }
func (titlePixelOverflowRule) Eval(ctx EvalContext) Finding {
	title := ctx.New.Title
	if strings.TrimSpace(title) == "" {
		return Finding{RuleID: "title_pixel_overflow"} // missing is title_changed's job
	}
	w := serpwidth.Width(title, TitleFontPx)
	if w > float64(TitleBudgetPx) {
		return Finding{RuleID: "title_pixel_overflow", Failed: true, Severity: model.SeverityWarning,
			Detail: overflowDetailJSON(w, TitleBudgetPx, title)}
	}
	return Finding{RuleID: "title_pixel_overflow"}
}

// meta_description_pixel_overflow — the meta description renders wider than the
// desktop SERP snippet container. Warning. Identical shape to the title rule at
// DescriptionFontPx / DescriptionBudgetPx; an empty description passes (missing is
// meta_description_changed's finding).
type metaDescriptionPixelOverflowRule struct{}

func (metaDescriptionPixelOverflowRule) ID() string { return "meta_description_pixel_overflow" }
func (metaDescriptionPixelOverflowRule) Eval(ctx EvalContext) Finding {
	desc := ctx.New.MetaDescription
	if strings.TrimSpace(desc) == "" {
		return Finding{RuleID: "meta_description_pixel_overflow"}
	}
	w := serpwidth.Width(desc, DescriptionFontPx)
	if w > float64(DescriptionBudgetPx) {
		return Finding{RuleID: "meta_description_pixel_overflow", Failed: true, Severity: model.SeverityWarning,
			Detail: overflowDetailJSON(w, DescriptionBudgetPx, desc)}
	}
	return Finding{RuleID: "meta_description_pixel_overflow"}
}
