package rules

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/richresult"
)

// richResultDetail is the shape of the Detail JSON the rich-result rules emit. The
// spec pins it to exactly {profile, type, entities, ineligible, missing}.
type richResultDetail struct {
	Profile    string   `json:"profile"`
	Type       string   `json:"type"`
	Entities   int      `json:"entities"`
	Ineligible int      `json:"ineligible"`
	Missing    []string `json:"missing"`
}

// productJSONLD is an eligible Product (name + offers). Helpers below mutate it.
const productEligibleJSONLD = `{"@context":"https://schema.org","@type":"Product","name":"Widget","offers":{"@type":"Offer","price":"9.99"}}`

// productIneligibleJSONLD drops offers/review/aggregateRating — Product loses
// eligibility (AnyOf group unsatisfied).
const productIneligibleJSONLD = `{"@context":"https://schema.org","@type":"Product","name":"Widget"}`

const articleEligibleJSONLD = `{"@type":"Article","headline":"Hello"}`
const articleIneligibleJSONLD = `{"@type":"Article","headline":""}`
const breadcrumbEligibleJSONLD = `{"@type":"BreadcrumbList","itemListElement":[{"@type":"ListItem","position":1}]}`
const breadcrumbIneligibleJSONLD = `{"@type":"BreadcrumbList","itemListElement":[]}`

// findRule locates a default rule by id (helper so the tests exercise the rules as
// wired into DefaultRuleSet, not a hand-built copy).
func findRule(t *testing.T, id string) Rule {
	t.Helper()
	for _, r := range DefaultRuleSet() {
		if r.ID() == id {
			return r
		}
	}
	t.Fatalf("rule %q not found in DefaultRuleSet()", id)
	return nil
}

// TestRichResultRulesRegistered asserts the four new rules are wired into the
// default set under exactly the spec ids.
func TestRichResultRulesRegistered(t *testing.T) {
	for _, id := range []string{
		"rich_result_product",
		"rich_result_article",
		"rich_result_breadcrumb",
		"structured_data_invalid_json",
	} {
		findRule(t, id) // fatals if missing
	}
}

// TestRichResultRuleAbsentPasses — a type the page does not implement is not a
// defect: absence passes (no issue held open for a retired/never-present type).
func TestRichResultRuleAbsentPasses(t *testing.T) {
	r := findRule(t, "rich_result_product")
	// New JSONLD carries only an Article; Product is absent.
	f := r.Eval(EvalContext{New: model.Snapshot{JSONLD: articleEligibleJSONLD}})
	if f.Failed {
		t.Fatalf("Product absent must pass, got Failed=true (%s)", f.Detail)
	}
}

// TestRichResultRuleEligiblePasses — a present, eligible Product passes.
func TestRichResultRuleEligiblePasses(t *testing.T) {
	r := findRule(t, "rich_result_product")
	f := r.Eval(EvalContext{New: model.Snapshot{JSONLD: productEligibleJSONLD}})
	if f.Failed {
		t.Fatalf("eligible Product must pass, got Failed=true (%s)", f.Detail)
	}
}

// TestRichResultLostEligibilityCritical — Old had ≥1 eligible Product, New has
// none eligible, and Old.ID != 0 → CRITICAL (a deploy that broke the markup).
func TestRichResultLostEligibilityCritical(t *testing.T) {
	r := findRule(t, "rich_result_product")
	f := r.Eval(EvalContext{
		Old: model.Snapshot{ID: 1, JSONLD: productEligibleJSONLD},
		New: model.Snapshot{JSONLD: productIneligibleJSONLD},
	})
	if !f.Failed {
		t.Fatalf("lost eligibility must fail")
	}
	if f.Severity != model.SeverityCritical {
		t.Fatalf("lost eligibility severity = %v, want critical", f.Severity)
	}
	var d richResultDetail
	if err := json.Unmarshal([]byte(f.Detail), &d); err != nil {
		t.Fatalf("Detail not JSON: %v (%q)", err, f.Detail)
	}
	if d.Profile != richresult.GRR202606.Version {
		t.Errorf("Detail.profile = %q, want %q", d.Profile, richresult.GRR202606.Version)
	}
	if d.Type != "Product" {
		t.Errorf("Detail.type = %q, want Product", d.Type)
	}
	if d.Entities != 1 || d.Ineligible != 1 {
		t.Errorf("Detail entities=%d ineligible=%d, want 1/1", d.Entities, d.Ineligible)
	}
	if len(d.Missing) == 0 {
		t.Errorf("Detail.missing empty; expected the missing AnyOf member(s)")
	}
}

// TestRichResultSteadyStateInvalidWarning — Old was ALSO ineligible (the crawl
// after a flip, or markup broken from a prior baseline): warning, not critical.
func TestRichResultSteadyStateInvalidWarning(t *testing.T) {
	r := findRule(t, "rich_result_product")
	f := r.Eval(EvalContext{
		Old: model.Snapshot{ID: 1, JSONLD: productIneligibleJSONLD},
		New: model.Snapshot{JSONLD: productIneligibleJSONLD},
	})
	if !f.Failed {
		t.Fatalf("steady-state invalid must fail")
	}
	if f.Severity != model.SeverityWarning {
		t.Fatalf("steady-state invalid severity = %v, want warning", f.Severity)
	}
}

// TestRichResultFirstCrawlInvalidWarning — Old.ID == 0 (first crawl), New
// ineligible: there is no prior baseline, so it is NOT a lost-eligibility flip →
// warning. (Bridging to Slack is separately suppressed by the first-crawl guard.)
func TestRichResultFirstCrawlInvalidWarning(t *testing.T) {
	r := findRule(t, "rich_result_product")
	f := r.Eval(EvalContext{
		Old: model.Snapshot{}, // ID == 0
		New: model.Snapshot{JSONLD: productIneligibleJSONLD},
	})
	if !f.Failed {
		t.Fatalf("first-crawl invalid must fail (opens a queryable issue)")
	}
	if f.Severity != model.SeverityWarning {
		t.Fatalf("first-crawl invalid severity = %v, want warning (no prior baseline = no flip)", f.Severity)
	}
}

// TestRichResultPartialEligibilityWarning — Old had an eligible Product but New
// still has at least one eligible Product among several: not a full loss of
// eligibility, so warning (partial), never critical.
func TestRichResultPartialEligibilityWarning(t *testing.T) {
	r := findRule(t, "rich_result_product")
	// Two products: one eligible, one not.
	newLD := `[` + productEligibleJSONLD + `,` + productIneligibleJSONLD + `]`
	f := r.Eval(EvalContext{
		Old: model.Snapshot{ID: 1, JSONLD: productEligibleJSONLD},
		New: model.Snapshot{JSONLD: newLD},
	})
	if !f.Failed {
		t.Fatalf("a partly-ineligible page must fail")
	}
	if f.Severity != model.SeverityWarning {
		t.Fatalf("partial eligibility severity = %v, want warning (still ≥1 eligible)", f.Severity)
	}
}

// TestRichResultOldHadNoEligibleEntityWarning — Old.ID != 0 but Old had ZERO
// eligible entities of the type (it was already broken). Not a flip from eligible
// → warning. Guards the "Old had ≥1 eligible entity" half of the critical clause.
func TestRichResultOldHadNoEligibleEntityWarning(t *testing.T) {
	r := findRule(t, "rich_result_product")
	f := r.Eval(EvalContext{
		Old: model.Snapshot{ID: 1, JSONLD: productIneligibleJSONLD}, // never eligible
		New: model.Snapshot{JSONLD: productIneligibleJSONLD},
	})
	if !f.Failed || f.Severity != model.SeverityWarning {
		t.Fatalf("Old never eligible => warning, got Failed=%v sev=%v", f.Failed, f.Severity)
	}
}

// TestRichResultArticleAndBreadcrumb covers the other two parameterized instances.
func TestRichResultArticleAndBreadcrumb(t *testing.T) {
	art := findRule(t, "rich_result_article")
	if f := art.Eval(EvalContext{New: model.Snapshot{JSONLD: articleEligibleJSONLD}}); f.Failed {
		t.Errorf("eligible Article must pass")
	}
	if f := art.Eval(EvalContext{Old: model.Snapshot{ID: 1, JSONLD: articleEligibleJSONLD}, New: model.Snapshot{JSONLD: articleIneligibleJSONLD}}); !f.Failed || f.Severity != model.SeverityCritical {
		t.Errorf("Article lost headline => critical, got Failed=%v sev=%v", f.Failed, f.Severity)
	}

	bc := findRule(t, "rich_result_breadcrumb")
	if f := bc.Eval(EvalContext{New: model.Snapshot{JSONLD: breadcrumbEligibleJSONLD}}); f.Failed {
		t.Errorf("eligible BreadcrumbList must pass")
	}
	if f := bc.Eval(EvalContext{Old: model.Snapshot{ID: 1, JSONLD: breadcrumbEligibleJSONLD}, New: model.Snapshot{JSONLD: breadcrumbIneligibleJSONLD}}); !f.Failed || f.Severity != model.SeverityCritical {
		t.Errorf("BreadcrumbList lost itemListElement => critical, got Failed=%v sev=%v", f.Failed, f.Severity)
	}
}

// TestRichResultTruncatedNoFinding — all four rules emit nothing on a truncated
// body (the h1_issue "don't guess on unextractable input" precedent): a severed
// <script> reads as malformed JSON or a vanished type.
func TestRichResultTruncatedNoFinding(t *testing.T) {
	cases := []struct {
		id  string
		ctx EvalContext
	}{
		{"rich_result_product", EvalContext{Old: model.Snapshot{ID: 1, JSONLD: productEligibleJSONLD}, New: model.Snapshot{JSONLD: productIneligibleJSONLD}, Truncated: true}},
		{"rich_result_article", EvalContext{Old: model.Snapshot{ID: 1, JSONLD: articleEligibleJSONLD}, New: model.Snapshot{JSONLD: articleIneligibleJSONLD}, Truncated: true}},
		{"rich_result_breadcrumb", EvalContext{Old: model.Snapshot{ID: 1, JSONLD: breadcrumbEligibleJSONLD}, New: model.Snapshot{JSONLD: breadcrumbIneligibleJSONLD}, Truncated: true}},
		{"structured_data_invalid_json", EvalContext{New: model.Snapshot{JSONLDInvalidCount: 3}, Truncated: true}},
	}
	for _, c := range cases {
		r := findRule(t, c.id)
		if f := r.Eval(c.ctx); f.Failed {
			t.Errorf("%s: truncated body must emit no finding, got Failed=true (%s)", c.id, f.Detail)
		}
	}
}

// TestStructuredDataInvalidJSONRule — fails (warning) iff JSONLDInvalidCount > 0.
func TestStructuredDataInvalidJSONRule(t *testing.T) {
	r := findRule(t, "structured_data_invalid_json")
	if f := r.Eval(EvalContext{New: model.Snapshot{JSONLDInvalidCount: 0}}); f.Failed {
		t.Errorf("invalid-count 0 must pass")
	}
	f := r.Eval(EvalContext{New: model.Snapshot{JSONLDInvalidCount: 2}})
	if !f.Failed {
		t.Fatalf("invalid-count 2 must fail")
	}
	if f.Severity != model.SeverityWarning {
		t.Errorf("severity = %v, want warning", f.Severity)
	}
}

// TestRichResultEngineRefreshOnFlip is acceptance criterion 5: the crawl AFTER a
// flip refreshes the open issue to warning, preserves OpenedAt, and is NOT returned
// as newly opened (no re-alert). Both baseline arms are exercised: the flip arm
// (Old.ID != 0, eligible) opens critical; the follow-up arm (Old.ID != 0, already
// ineligible) refreshes to warning. We assert through the same before/after open-set
// diff the production procDeps.ApplyRules uses.
func TestRichResultEngineRefreshOnFlip(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)

	fs := newFakeIssueStore()
	eng := NewEngine(DefaultRuleSet(), fs, func() time.Time { return t0 })

	// --- Crawl 1: the flip. Old eligible (ID != 0) -> New ineligible => critical, NEWLY opened.
	openBefore := openRuleSet(fs, 7)
	if err := eng.Apply(ctx, EvalContext{
		URLID: 7, Importance: 1.0,
		Old: snapWithJSONLD(1, productEligibleJSONLD),
		New: snapWithJSONLD(0, productIneligibleJSONLD),
	}); err != nil {
		t.Fatalf("Apply crawl1: %v", err)
	}
	openAfter := openRuleSet(fs, 7)
	if openBefore["rich_result_product"] {
		t.Fatal("precondition: rule must not be open before crawl1")
	}
	if !openAfter["rich_result_product"] {
		t.Fatal("crawl1: lost-eligibility must open rich_result_product")
	}
	iss1 := fs.open[key(7, "rich_result_product")]
	if iss1.Severity != model.SeverityCritical {
		t.Fatalf("crawl1 severity = %v, want critical", iss1.Severity)
	}
	if !iss1.OpenedAt.Equal(t0) {
		t.Fatalf("crawl1 OpenedAt = %v, want %v", iss1.OpenedAt, t0)
	}

	// --- Crawl 2: the follow-up. Now Old is ALSO ineligible (Old.ID != 0). The
	// rule refreshes to WARNING; the issue must already be open (not newly opened).
	eng2 := NewEngine(DefaultRuleSet(), fs, func() time.Time { return t1 })
	openBefore2 := openRuleSet(fs, 7)
	if !openBefore2["rich_result_product"] {
		t.Fatal("crawl2 precondition: issue must already be open")
	}
	if err := eng2.Apply(ctx, EvalContext{
		URLID: 7, Importance: 1.0,
		Old: snapWithJSONLD(2, productIneligibleJSONLD),
		New: snapWithJSONLD(0, productIneligibleJSONLD),
	}); err != nil {
		t.Fatalf("Apply crawl2: %v", err)
	}
	iss2 := fs.open[key(7, "rich_result_product")]
	if iss2.Severity != model.SeverityWarning {
		t.Fatalf("crawl2 severity = %v, want warning (steady-state refresh)", iss2.Severity)
	}
	if !iss2.OpenedAt.Equal(t0) {
		t.Fatalf("crawl2 OpenedAt = %v, want preserved %v (no re-open)", iss2.OpenedAt, t0)
	}
	if !iss2.LastSeenAt.Equal(t1) {
		t.Fatalf("crawl2 LastSeenAt = %v, want advanced to %v", iss2.LastSeenAt, t1)
	}
	// "NOT returned as newly opened": the before-set already contained the rule, so a
	// production before/after diff yields it in neither newly-opened slice.
	if !openRuleSet(fs, 7)["rich_result_product"] {
		t.Fatal("crawl2: issue must remain open")
	}
}

// snapWithJSONLD builds a minimal Snapshot carrying only an ID and a JSONLD column,
// with the other rule-relevant fields set to non-failing values so the engine does
// not open unrelated issues that pollute the open-set assertions.
func snapWithJSONLD(id int64, jsonld string) model.Snapshot {
	return model.Snapshot{
		ID:                 id,
		Title:              "T",
		Canonical:          "https://ex.com/p",
		MetaRobots:         "index,follow",
		MetaDescription:    "d",
		Headings:           `{"h1":["x"]}`,
		HTTPStatus:         200,
		Indexable:          true,
		IndexabilityReason: "indexable",
		JSONLD:             jsonld,
	}
}

// openRuleSet returns the set of rule_ids with an open issue for urlID.
func openRuleSet(fs *fakeIssueStore, urlID int64) map[string]bool {
	out := map[string]bool{}
	for k, iss := range fs.open {
		_ = k
		if iss.URLID == urlID && iss.Status == model.IssueOpen {
			out[iss.RuleID] = true
		}
	}
	return out
}
