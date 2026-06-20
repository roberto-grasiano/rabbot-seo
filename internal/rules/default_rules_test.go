package rules

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/robotsmeta"
)

func findingFor(rs []Rule, id string, ctx EvalContext) Finding {
	for _, r := range rs {
		if r.ID() == id {
			return r.Eval(ctx)
		}
	}
	return Finding{}
}

func TestIndexabilityFlipRule(t *testing.T) {
	r := indexabilityFlipRule{}
	failing := r.Eval(EvalContext{
		New:     model.Snapshot{Indexable: false, IndexabilityReason: "meta noindex"},
		Old:     model.Snapshot{ID: 1, Indexable: true},
		Changes: []model.Change{{Field: "indexable", OldValue: "true", NewValue: "false"}},
	})
	if !failing.Failed || failing.Severity != model.SeverityCritical {
		t.Errorf("indexable true->false must be critical failure, got %+v", failing)
	}
	passing := r.Eval(EvalContext{New: model.Snapshot{Indexable: true}, Old: model.Snapshot{ID: 1, Indexable: true}})
	if passing.Failed {
		t.Errorf("still indexable must pass, got %+v", passing)
	}
}

// TestIndexabilityFlipRule_BaselineGuard covers F6: indexability_flip is a *flip*
// rule and must require a prior INDEXABLE baseline before firing. On the very
// first crawl (Old.ID == 0) a legitimately-noindex page (/cart, /login, paginated
// archive) must NOT open a spurious CRITICAL issue, and a page that was already
// non-indexable on the prior crawl has not flipped either. Only a genuine
// transition (Old indexable -> New non-indexable) is a flip.
func TestIndexabilityFlipRule_BaselineGuard(t *testing.T) {
	r := indexabilityFlipRule{}
	tests := []struct {
		name         string
		oldID        int64
		oldIndexable bool
		newIndexable bool
		wantFail     bool
	}{
		{"first crawl of noindex page does not fire", 0, false, false, false},
		{"first crawl of indexable page passes", 0, false, true, false},
		{"genuine flip indexable->noindex fires", 1, true, false, true},
		{"still indexable passes", 1, true, true, false},
		{"already non-indexable on prior crawl does not fire", 1, false, false, false},
		{"recovered noindex->indexable passes", 1, false, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := r.Eval(EvalContext{
				New: model.Snapshot{Indexable: tc.newIndexable, IndexabilityReason: "meta noindex"},
				Old: model.Snapshot{ID: tc.oldID, Indexable: tc.oldIndexable},
			})
			if f.Failed != tc.wantFail {
				t.Errorf("oldID=%d oldIdx=%v newIdx=%v: Failed=%v, want %v (%+v)",
					tc.oldID, tc.oldIndexable, tc.newIndexable, f.Failed, tc.wantFail, f)
			}
			if tc.wantFail && f.Severity != model.SeverityCritical {
				t.Errorf("indexability flip must be critical, got %v", f.Severity)
			}
		})
	}
}

func TestStatusRegressionRule(t *testing.T) {
	r := statusRegressionRule{}
	f := r.Eval(EvalContext{New: model.Snapshot{HTTPStatus: 500}, Old: model.Snapshot{ID: 1, HTTPStatus: 200}})
	if !f.Failed || f.Severity != model.SeverityCritical {
		t.Errorf("200->500 must be critical, got %+v", f)
	}
	ok := r.Eval(EvalContext{New: model.Snapshot{HTTPStatus: 200}, Old: model.Snapshot{ID: 1, HTTPStatus: 200}})
	if ok.Failed {
		t.Errorf("200 must pass, got %+v", ok)
	}
}

func TestMetaRobotsNoindexRule(t *testing.T) {
	r := metaRobotsNoindexRule{}
	f := r.Eval(EvalContext{New: model.Snapshot{MetaRobots: "noindex, follow"}})
	if !f.Failed || f.Severity != model.SeverityCritical {
		t.Errorf("noindex must be critical, got %+v", f)
	}
	if !strings.Contains(strings.ToLower(f.Detail), "noindex") {
		t.Errorf("detail should mention noindex: %q", f.Detail)
	}
	ok := r.Eval(EvalContext{New: model.Snapshot{MetaRobots: "index,follow"}})
	if ok.Failed {
		t.Errorf("index,follow must pass, got %+v", ok)
	}
}

// TestMetaRobotsNoindexRule_TokenBoundary covers R1: the noindex rule must fire
// ONLY on a real `noindex` directive matched on token boundaries — never on
// `nofollow` (which controls link equity, not indexability) and never on a
// substring inside another token.
func TestMetaRobotsNoindexRule_TokenBoundary(t *testing.T) {
	r := metaRobotsNoindexRule{}
	tests := []struct {
		name       string
		metaRobots string
		xRobots    string
		wantFail   bool
	}{
		{"index,nofollow not a noindex regression", "index,nofollow", "", false},
		{"nofollow alone passes", "nofollow", "", false},
		{"noindex,follow is critical", "noindex,follow", "", true},
		{"index,follow passes", "index,follow", "", false},
		{"uppercase NOINDEX fails", "NOINDEX, FOLLOW", "", true},
		{"spaced noindex token fails", "  noindex  ", "", true},
		{"substring noindexable must not match", "noindexable", "", false},
		{"prefixed substring must not match", "xnoindex", "", false},
		{"x-robots-tag noindex fails", "", "googlebot: noindex", true},
		{"x-robots-tag nofollow passes", "", "googlebot: nofollow", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := r.Eval(EvalContext{New: model.Snapshot{MetaRobots: tc.metaRobots, XRobotsTag: tc.xRobots}})
			if f.Failed != tc.wantFail {
				t.Errorf("MetaRobots=%q XRobots=%q: Failed=%v, want %v (%+v)", tc.metaRobots, tc.xRobots, f.Failed, tc.wantFail, f)
			}
			if tc.wantFail && f.Severity != model.SeverityCritical {
				t.Errorf("noindex must be critical, got %v", f.Severity)
			}
		})
	}
}

// TestMetaRobotsNoindexRule_AgreesWithRobotsmeta covers fix (a): the alert rule
// and the indexability verdict must share ONE noindex parse (internal/robotsmeta)
// so they can never drift. The rule now delegates to robotsmeta.IsNoindex; this
// cross-check pins that the rule's failing verdict EQUALS robotsmeta.IsNoindex for
// the two forms most likely to diverge under a hand-rolled tokenizer:
// "googlebot: noindex" (a user-agent-prefixed X-Robots-Tag) and "none" (which
// Google defines as noindex+nofollow). Both must FAIL via either field, and both
// must be reported noindex by robotsmeta.
func TestMetaRobotsNoindexRule_AgreesWithRobotsmeta(t *testing.T) {
	r := metaRobotsNoindexRule{}
	for _, value := range []string{"googlebot: noindex", "none", "googlebot: none", "index, follow", "nofollow"} {
		t.Run(value, func(t *testing.T) {
			want := robotsmeta.IsNoindex(value)
			// The rule must read the SAME way whether the directive arrives via
			// meta robots or the X-Robots-Tag header.
			viaMeta := r.Eval(EvalContext{New: model.Snapshot{MetaRobots: value}})
			if viaMeta.Failed != want {
				t.Errorf("MetaRobots=%q: rule.Failed=%v but robotsmeta.IsNoindex=%v — rule and verdict must not drift", value, viaMeta.Failed, want)
			}
			viaHeader := r.Eval(EvalContext{New: model.Snapshot{XRobotsTag: value}})
			if viaHeader.Failed != want {
				t.Errorf("XRobotsTag=%q: rule.Failed=%v but robotsmeta.IsNoindex=%v — rule and verdict must not drift", value, viaHeader.Failed, want)
			}
		})
	}
}

// TestH1IssueRule covers R2: H1 detection must parse the structured Headings JSON
// and count h1 entries, not substring-match the JSON text.
func TestH1IssueRule(t *testing.T) {
	r := h1IssueRule{}
	tests := []struct {
		name     string
		headings string
		wantFail bool
		wantSub  string // substring expected in Detail when failing
	}{
		{"missing h1", `{"h2":["x"]}`, true, "missing"},
		{"single h1 ok", `{"h1":["Hello"]}`, false, ""},
		{"multiple h1", `{"h1":["a","b"]}`, true, "multiple"},
		{"empty headings yields no finding", "", false, ""},
		{"whitespace-only headings yields no finding", "   ", false, ""},
		{"invalid json yields no finding", "{not json", false, ""},
		{"empty h1 array is missing", `{"h1":[]}`, true, "missing"},
		// Regression for the old substring bug: body text mentioning "h1" must
		// not be mistaken for a present h1 when the structured h1 is absent.
		{"h2 text mentioning h1 still missing", `{"h2":["see the h1 below"]}`, true, "missing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := r.Eval(EvalContext{New: model.Snapshot{Headings: tc.headings}})
			if f.Failed != tc.wantFail {
				t.Errorf("Headings=%q: Failed=%v, want %v (%+v)", tc.headings, f.Failed, tc.wantFail, f)
			}
			if tc.wantFail {
				// Multiple H1s are not an SEO error under current Google guidance, so the
				// "multiple" sub-finding is info (recorded, never pages); a missing H1 stays warning.
				wantSev := model.SeverityWarning
				if tc.wantSub == "multiple" {
					wantSev = model.SeverityInfo
				}
				if f.Severity != wantSev {
					t.Errorf("h1 %q: severity=%v, want %v", tc.wantSub, f.Severity, wantSev)
				}
				if tc.wantSub != "" && !strings.Contains(f.Detail, tc.wantSub) {
					t.Errorf("detail %q should contain %q", f.Detail, tc.wantSub)
				}
			}
		})
	}
}

// TestH1IssueRule_ChangedWhenSingle covers the "headings changed" branch: a single
// h1 present but the headings field changed => warning.
func TestH1IssueRule_ChangedWhenSingle(t *testing.T) {
	r := h1IssueRule{}
	f := r.Eval(EvalContext{
		New:     model.Snapshot{Headings: `{"h1":["New"]}`},
		Changes: []model.Change{{Field: "headings", OldValue: `{"h1":["Old"]}`, NewValue: `{"h1":["New"]}`}},
	})
	if !f.Failed || f.Severity != model.SeverityWarning {
		t.Errorf("single h1 with headings change must warn, got %+v", f)
	}
	if !strings.Contains(f.Detail, "changed") {
		t.Errorf("detail should mention changed: %q", f.Detail)
	}
}

// TestH1IssueRule_HeadingChangeWinsOverMultiple covers the H1 WARNING SHADOW
// regression (PR #95): a real heading rewrite must WARN regardless of H1 count.
// Before the fix, the `n > 1` INFO "multiple" arm returned before the
// hasChange("headings") WARNING branch, so any 2+-H1 page silently downgraded a
// genuine heading rewrite to info and never paged. The contract:
//   - 2+ H1s WITH a headings change => WARNING (the rewrite pages, not info).
//   - 2+ H1s with NO headings change => INFO "multiple" (steady-state count only).
//   - single H1 with a headings change => WARNING (unchanged behavior).
//   - missing H1 stays WARNING regardless of changes.
func TestH1IssueRule_HeadingChangeWinsOverMultiple(t *testing.T) {
	r := h1IssueRule{}

	// 2+ H1s AND a real heading rewrite: must WARN (not the info "multiple" shadow).
	multiChanged := r.Eval(EvalContext{
		New:     model.Snapshot{Headings: `{"h1":["TOTALLY","NEW"]}`},
		Changes: []model.Change{{Field: "headings", OldValue: `{"h1":["A","B"]}`, NewValue: `{"h1":["TOTALLY","NEW"]}`}},
	})
	if !multiChanged.Failed || multiChanged.Severity != model.SeverityWarning {
		t.Fatalf("2+ H1 with a headings change must WARN (not info), got %+v", multiChanged)
	}
	if !strings.Contains(multiChanged.Detail, "changed") {
		t.Errorf("detail should mark the change, got %q", multiChanged.Detail)
	}

	// 2+ H1s with NO change: steady-state INFO "multiple".
	multiSteady := r.Eval(EvalContext{New: model.Snapshot{Headings: `{"h1":["a","b"]}`}})
	if !multiSteady.Failed || multiSteady.Severity != model.SeverityInfo {
		t.Fatalf("2+ H1 with no change must be INFO multiple, got %+v", multiSteady)
	}
	if !strings.Contains(multiSteady.Detail, "multiple") {
		t.Errorf("steady multiple detail should say multiple, got %q", multiSteady.Detail)
	}

	// Single H1 with a heading change still WARNS.
	singleChanged := r.Eval(EvalContext{
		New:     model.Snapshot{Headings: `{"h1":["New"]}`},
		Changes: []model.Change{{Field: "headings", OldValue: `{"h1":["Old"]}`, NewValue: `{"h1":["New"]}`}},
	})
	if !singleChanged.Failed || singleChanged.Severity != model.SeverityWarning {
		t.Errorf("single H1 with a headings change must WARN, got %+v", singleChanged)
	}

	// Missing H1 stays WARNING even when headings changed (the n==0 arm wins).
	missingChanged := r.Eval(EvalContext{
		New:     model.Snapshot{Headings: `{"h2":["x"]}`},
		Changes: []model.Change{{Field: "headings", OldValue: `{"h1":["Old"]}`, NewValue: `{"h2":["x"]}`}},
	})
	if !missingChanged.Failed || missingChanged.Severity != model.SeverityWarning {
		t.Errorf("missing H1 must stay WARNING, got %+v", missingChanged)
	}
	if !strings.Contains(missingChanged.Detail, "missing") {
		t.Errorf("missing detail should say missing, got %q", missingChanged.Detail)
	}
}

// TestStatusRegressionRule_Born4xx covers the BORN-4xx MISS: a URL that is 4xx
// from its FIRST crawl (Old.ID == 0) never fired before, because the 4xx arm
// required a prior 2xx/3xx baseline — so a page born broken was invisible forever.
// The fix mirrors the rich_result first-crawl idiom: open at WARNING on born-4xx
// and on steady 4xx (so the issue stays open and auto-closes on recovery), while
// keeping 2xx/3xx->4xx and any 5xx CRITICAL. First-crawl Slack suppression is
// handled by ProcessFetch's first-crawl guard, not here.
func TestStatusRegressionRule_Born4xx(t *testing.T) {
	r := statusRegressionRule{}
	tests := []struct {
		name     string
		oldID    int64
		oldCode  int
		newCode  int
		wantFail bool
		wantSev  model.Severity
	}{
		{"born 404 opens warning", 0, 0, 404, true, model.SeverityWarning},
		{"stays 404 open warning crawl 2", 1, 404, 404, true, model.SeverityWarning},
		{"404 recovers to 200 closes", 1, 404, 200, false, ""},
		{"200 -> 404 is critical regression", 1, 200, 404, true, model.SeverityCritical},
		{"3xx -> 404 is critical regression", 1, 301, 404, true, model.SeverityCritical},
		{"born 5xx critical", 0, 0, 503, true, model.SeverityCritical},
		{"200 -> 500 critical", 1, 200, 500, true, model.SeverityCritical},
		{"steady 500 stays critical", 1, 500, 500, true, model.SeverityCritical},
		{"born 200 passes", 0, 0, 200, false, ""},
		{"steady 200 passes", 1, 200, 200, false, ""},
		{"4xx -> 5xx escalates to critical", 1, 404, 500, true, model.SeverityCritical},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := r.Eval(EvalContext{
				New: model.Snapshot{HTTPStatus: tc.newCode},
				Old: model.Snapshot{ID: tc.oldID, HTTPStatus: tc.oldCode},
			})
			if f.Failed != tc.wantFail {
				t.Fatalf("oldID=%d old=%d new=%d: Failed=%v, want %v (%+v)", tc.oldID, tc.oldCode, tc.newCode, f.Failed, tc.wantFail, f)
			}
			if tc.wantFail {
				if f.Severity != tc.wantSev {
					t.Errorf("oldID=%d old=%d new=%d: severity=%v, want %v", tc.oldID, tc.oldCode, tc.newCode, f.Severity, tc.wantSev)
				}
				if f.RuleID != "status_regression" {
					t.Errorf("RuleID must stay status_regression (no re-baseline), got %q", f.RuleID)
				}
				assertDetailIsJSON(t, f.Detail)
			}
		})
	}
}

// TestBrokenLinksSpikeRule covers R-tests: internal link count dropping >30% vs
// prior fails; no/insufficient drop or no prior passes.
func TestBrokenLinksSpikeRule(t *testing.T) {
	r := brokenLinksSpikeRule{}
	tests := []struct {
		name     string
		oldID    int64
		oldCount int
		newCount int
		wantFail bool
	}{
		{"no prior snapshot", 0, 0, 5, false},
		{"prior had zero links", 1, 0, 0, false},
		{"sharp drop 100->50 fails", 1, 100, 50, true},
		{"exactly 30% does not fire", 1, 100, 70, false},
		{"just over 30% fires", 1, 100, 69, true},
		{"links grew passes", 1, 50, 80, false},
		{"links unchanged passes", 1, 40, 40, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := r.Eval(EvalContext{
				New: model.Snapshot{InternalLinkCount: tc.newCount},
				Old: model.Snapshot{ID: tc.oldID, InternalLinkCount: tc.oldCount},
			})
			if f.Failed != tc.wantFail {
				t.Errorf("old=%d new=%d (oldID=%d): Failed=%v, want %v", tc.oldCount, tc.newCount, tc.oldID, f.Failed, tc.wantFail)
			}
			if tc.wantFail && f.Severity != model.SeverityWarning {
				t.Errorf("broken-links spike must be warning, got %v", f.Severity)
			}
		})
	}
}

// TestHreflangInvalidRule covers #16: hreflang_invalid is HONEST — it fires
// warning-on-change only. The previously-documented CRITICAL path (a "hreflang"
// substring in IndexabilityReason) was DEAD: nothing ever wrote that token and
// hreflang is stored as a raw lang array with no validity check, so the rule
// could never fire critical. Per the locked decision ("downgrade the doc now,
// validation is fast-follow") the dead critical branch is removed; real BCP-47
// validation is a deferred fast-follow. A hreflang set change is a warning;
// otherwise it passes.
func TestHreflangInvalidRule(t *testing.T) {
	r := hreflangInvalidRule{}

	changed := r.Eval(EvalContext{
		New:     model.Snapshot{IndexabilityReason: "ok"},
		Changes: []model.Change{{Field: "hreflang", OldValue: "en", NewValue: "en,de"}},
	})
	if !changed.Failed || changed.Severity != model.SeverityWarning {
		t.Errorf("hreflang change must be warning, got %+v", changed)
	}

	ok := r.Eval(EvalContext{New: model.Snapshot{IndexabilityReason: "indexable"}})
	if ok.Failed {
		t.Errorf("valid, unchanged hreflang must pass, got %+v", ok)
	}
}

// TestHreflangInvalidRule_NoDeadCriticalBranch covers #16: the dead critical
// branch must be gone. An IndexabilityReason that merely CONTAINS "hreflang"
// (the old, unreachable critical trigger) must NOT fire — the rule may only fire
// warning, and only on a real hreflang change. A rule that advertises a CRITICAL
// it can never reach is dishonest; this test makes re-adding that branch fail.
func TestHreflangInvalidRule_NoDeadCriticalBranch(t *testing.T) {
	r := hreflangInvalidRule{}

	// "hreflang" in the reason, but NO change: must pass (no spurious critical).
	noChange := r.Eval(EvalContext{New: model.Snapshot{IndexabilityReason: "hreflang return tag missing"}})
	if noChange.Failed {
		t.Errorf("a 'hreflang' substring in IndexabilityReason must NOT fire (dead critical branch removed), got %+v", noChange)
	}

	// "hreflang" in the reason AND a real change: fires, but only WARNING — never
	// the removed critical.
	withChange := r.Eval(EvalContext{
		New:     model.Snapshot{IndexabilityReason: "hreflang return tag missing"},
		Changes: []model.Change{{Field: "hreflang", OldValue: "en", NewValue: "en,de"}},
	})
	if !withChange.Failed {
		t.Fatalf("a real hreflang change must fire, got %+v", withChange)
	}
	if withChange.Severity != model.SeverityWarning {
		t.Errorf("hreflang_invalid must only ever fire WARNING (no critical), got %v (%+v)", withChange.Severity, withChange)
	}
}

// TestMetaDescriptionChangedRule covers R-tests: missing description warns,
// a description change warns, otherwise it passes.
func TestMetaDescriptionChangedRule(t *testing.T) {
	r := metaDescriptionChangedRule{}

	missing := r.Eval(EvalContext{New: model.Snapshot{MetaDescription: "   "}})
	if !missing.Failed || missing.Severity != model.SeverityWarning {
		t.Errorf("missing meta description must warn, got %+v", missing)
	}

	changed := r.Eval(EvalContext{
		New:     model.Snapshot{MetaDescription: "new copy"},
		Changes: []model.Change{{Field: "meta_description", OldValue: "old", NewValue: "new copy"}},
	})
	if !changed.Failed || changed.Severity != model.SeverityWarning {
		t.Errorf("meta description change must warn, got %+v", changed)
	}

	ok := r.Eval(EvalContext{New: model.Snapshot{MetaDescription: "stable copy"}})
	if ok.Failed {
		t.Errorf("present, unchanged meta description must pass, got %+v", ok)
	}
}

// TestMetaDescriptionChangedRule_MissingReadsAsAbsent covers #86: a MISSING
// meta description must surface as an ABSENT-field finding, not as a diff. The
// missing-branch Detail must (a) be valid JSON, (b) carry an explicit
// "absent" marker so an operator/agent never reads it as a change, and (c)
// stay clearly distinct from the changed-branch Detail. The RuleID is
// deliberately UNCHANGED (still meta_description_changed) so existing open
// issues keep their key and auto-close — only the detail wording clarifies.
func TestMetaDescriptionChangedRule_MissingReadsAsAbsent(t *testing.T) {
	r := metaDescriptionChangedRule{}

	missing := r.Eval(EvalContext{New: model.Snapshot{MetaDescription: "   "}})
	if !missing.Failed || missing.Severity != model.SeverityWarning {
		t.Fatalf("missing meta description must warn, got %+v", missing)
	}
	if missing.RuleID != "meta_description_changed" {
		t.Errorf("RuleID must stay meta_description_changed (no re-baseline), got %q", missing.RuleID)
	}
	assertDetailIsJSON(t, missing.Detail)
	// The disambiguation marker (issue #86): a missing field must not read like a
	// change. "absent" makes the field's state unambiguous.
	if !strings.Contains(missing.Detail, "absent") {
		t.Errorf("missing meta_description detail must mark the field as absent, got %q", missing.Detail)
	}

	// The missing detail and the changed detail must differ so they can never be
	// confused for one another.
	changed := r.Eval(EvalContext{
		New:     model.Snapshot{MetaDescription: "new copy"},
		Changes: []model.Change{{Field: "meta_description", OldValue: "old", NewValue: "new copy"}},
	})
	if missing.Detail == changed.Detail {
		t.Errorf("missing and changed details must differ; both = %q", missing.Detail)
	}
	// A real diff must NOT carry the absent marker.
	if strings.Contains(changed.Detail, "absent") {
		t.Errorf("a genuine meta_description change must not be marked absent, got %q", changed.Detail)
	}
}

func TestTitleChangedRule(t *testing.T) {
	r := titleChangedRule{}
	missing := r.Eval(EvalContext{New: model.Snapshot{Title: ""}})
	if !missing.Failed {
		t.Errorf("missing title must fail, got %+v", missing)
	}
	changed := r.Eval(EvalContext{
		New:     model.Snapshot{Title: "B"},
		Changes: []model.Change{{Field: "title", OldValue: "A", NewValue: "B"}},
	})
	if !changed.Failed || changed.Severity != model.SeverityWarning {
		t.Errorf("title change must be warning, got %+v", changed)
	}
	ok := r.Eval(EvalContext{New: model.Snapshot{Title: "Stable"}})
	if ok.Failed {
		t.Errorf("unchanged present title must pass, got %+v", ok)
	}
}

// TestTitleChangedRule_MissingReadsAsAbsent covers #86 for the title rule (same
// shape as the meta-description case): a MISSING title must read as an
// ABSENT-field finding, not a diff. RuleID stays title_changed (no re-baseline).
func TestTitleChangedRule_MissingReadsAsAbsent(t *testing.T) {
	r := titleChangedRule{}

	missing := r.Eval(EvalContext{New: model.Snapshot{Title: "   "}})
	if !missing.Failed || missing.Severity != model.SeverityWarning {
		t.Fatalf("missing title must warn, got %+v", missing)
	}
	if missing.RuleID != "title_changed" {
		t.Errorf("RuleID must stay title_changed (no re-baseline), got %q", missing.RuleID)
	}
	assertDetailIsJSON(t, missing.Detail)
	if !strings.Contains(missing.Detail, "absent") {
		t.Errorf("missing title detail must mark the field as absent, got %q", missing.Detail)
	}

	changed := r.Eval(EvalContext{
		New:     model.Snapshot{Title: "B"},
		Changes: []model.Change{{Field: "title", OldValue: "A", NewValue: "B"}},
	})
	if missing.Detail == changed.Detail {
		t.Errorf("missing and changed title details must differ; both = %q", missing.Detail)
	}
	if strings.Contains(changed.Detail, "absent") {
		t.Errorf("a genuine title change must not be marked absent, got %q", changed.Detail)
	}
}

func TestCanonicalChangedRule(t *testing.T) {
	r := canonicalChangedRule{}
	missing := r.Eval(EvalContext{New: model.Snapshot{Canonical: ""}})
	if !missing.Failed || missing.Severity != model.SeverityCritical {
		t.Errorf("missing canonical must be critical, got %+v", missing)
	}
	changed := r.Eval(EvalContext{
		New:     model.Snapshot{Canonical: "https://x/b"},
		Changes: []model.Change{{Field: "canonical", OldValue: "https://x/a", NewValue: "https://x/b"}},
	})
	if !changed.Failed {
		t.Errorf("canonical change must fail, got %+v", changed)
	}
}

// TestNeedsRenderingRule (A8, acceptance #6) — the needs_rendering rule FAILS at the
// warning tier when the page's persisted render_mode is client_shell or
// head_only_shell, and PASSES (Failed=false, so the engine lifecycle CLOSES the issue)
// for every recoverable / steady-state mode. Both failure details must DISTINGUISH the
// two shell kinds: head_only_shell means the head is monitored but the body is not;
// client_shell means nothing beyond fetch status is monitored. Crucially this rule
// keys off the persisted RenderMode (a STORED column), so the test drives it with the
// exact model.RenderMode values LatestSnapshot scans back, not a logical proxy.
func TestNeedsRenderingRule(t *testing.T) {
	r := needsRenderingRule{}

	t.Run("client_shell fails warning, not-monitored detail", func(t *testing.T) {
		f := r.Eval(EvalContext{New: model.Snapshot{RenderMode: model.RenderClientShell}})
		if !f.Failed {
			t.Fatalf("client_shell must fail, got %+v", f)
		}
		if f.Severity != model.SeverityWarning {
			t.Errorf("client_shell severity = %q, want warning", f.Severity)
		}
		if !strings.Contains(f.Detail, "client_shell") {
			t.Errorf("client_shell detail must name the mode, got %q", f.Detail)
		}
		assertDetailIsJSON(t, f.Detail)
	})

	t.Run("head_only_shell fails warning, head-monitored detail DISTINCT from client_shell", func(t *testing.T) {
		head := r.Eval(EvalContext{New: model.Snapshot{RenderMode: model.RenderHeadOnlyShell}})
		if !head.Failed || head.Severity != model.SeverityWarning {
			t.Fatalf("head_only_shell must fail at warning, got %+v", head)
		}
		if !strings.Contains(head.Detail, "head_only_shell") {
			t.Errorf("head_only_shell detail must name the mode, got %q", head.Detail)
		}
		assertDetailIsJSON(t, head.Detail)
		// The two shell details MUST differ: head_only_shell = head monitored, body not;
		// client_shell = nothing beyond fetch status. A shared detail would erase the
		// operator-facing distinction acceptance #6 requires.
		client := r.Eval(EvalContext{New: model.Snapshot{RenderMode: model.RenderClientShell}})
		if head.Detail == client.Detail {
			t.Errorf("head_only_shell and client_shell details must differ; both = %q", head.Detail)
		}
	})

	// PASSES arm: every recoverable / steady-state mode closes the issue (Failed=false).
	// "" (pre-A8 rows) and unknown must NOT fire — they are not findings. server_rendered
	// and hydrated are the recovery targets: a page that returns to them closes the issue
	// via the engine lifecycle.
	t.Run("recoverable and steady-state modes pass", func(t *testing.T) {
		for _, rm := range []model.RenderMode{
			model.RenderServerRendered,
			model.RenderHydrated,
			model.RenderUnknown,
			"", // pre-A8 / zero value
		} {
			f := r.Eval(EvalContext{New: model.Snapshot{RenderMode: rm}})
			if f.Failed {
				t.Errorf("render_mode=%q must PASS (close the issue), got %+v", rm, f)
			}
			if f.RuleID != "needs_rendering" {
				t.Errorf("render_mode=%q: RuleID = %q, want needs_rendering", rm, f.RuleID)
			}
		}
	})
}

// TestNeedsRenderingRule_Registered guards that the rule is wired into DefaultRuleSet
// under the snake_case id, so the engine actually evaluates it (a rule that exists but
// is unregistered silently never opens an issue).
func TestNeedsRenderingRule_Registered(t *testing.T) {
	f := findingFor(DefaultRuleSet(), "needs_rendering",
		EvalContext{New: model.Snapshot{RenderMode: model.RenderClientShell}})
	if f.RuleID != "needs_rendering" {
		t.Fatalf("needs_rendering not registered in DefaultRuleSet (got RuleID %q)", f.RuleID)
	}
	if !f.Failed || f.Severity != model.SeverityWarning {
		t.Errorf("registered needs_rendering on client_shell must fail warning, got %+v", f)
	}
}

// TestNeedsRenderingRuleMatchesIsShell pins the rule's failing set to
// model.RenderMode.IsShell across EVERY render mode (the formula↔constant drift
// guard). The alert-resolution path (Processor.resolveHealthyFields) treats
// leaving IsShell as the recovery that closes the bridged render_mode incident;
// if the rule's failing set and IsShell ever diverged, a page could open a
// needs_rendering alert that never auto-resolves (or resolve one that never
// opened). This test makes that divergence impossible to merge.
func TestNeedsRenderingRuleMatchesIsShell(t *testing.T) {
	r := needsRenderingRule{}
	for _, rm := range []model.RenderMode{
		model.RenderServerRendered,
		model.RenderHydrated,
		model.RenderHeadOnlyShell,
		model.RenderClientShell,
		model.RenderUnknown,
		"", // pre-A8 / zero value
	} {
		f := r.Eval(EvalContext{New: model.Snapshot{RenderMode: rm}})
		if f.Failed != rm.IsShell() {
			t.Errorf("render_mode=%q: rule.Failed=%v but IsShell=%v — the rule's open condition must equal IsShell so recovery resolution stays in lockstep",
				rm, f.Failed, rm.IsShell())
		}
	}
}

func assertDetailIsJSON(t *testing.T, detail string) {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(detail), &v); err != nil {
		t.Errorf("Detail must be valid JSON, got %q (%v)", detail, err)
	}
}

func TestDefaultRuleSetIsRegistered_Compiles(t *testing.T) {
	rs := DefaultRuleSet()
	_ = findingFor(rs, "h1_issue", EvalContext{New: model.Snapshot{}})
	if len(rs) == 0 {
		t.Fatal("empty rule set")
	}
}
